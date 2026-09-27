package app

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubQuota lets a test declare which account IDs upstream reports as
// exhausted, without standing up the whole quota service.
type stubQuota struct {
	mu      sync.Mutex
	blocked map[string]bool
}

func newStubQuota(blocked ...string) *stubQuota {
	s := &stubQuota{blocked: map[string]bool{}}
	for _, id := range blocked {
		s.blocked[id] = true
	}
	return s
}

func (s *stubQuota) set(id string, blocked bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blocked[id] = blocked
}

func (s *stubQuota) AccountBlocked(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blocked[id]
}

func poolOf(t *testing.T, keys ...string) *AccountPool {
	t.Helper()
	list := make([]AccountConfig, 0, len(keys))
	for i, k := range keys {
		list = append(list, AccountConfig{Name: string(rune('a' + i)), APIKey: k})
	}
	return NewAccountPool(list)
}

// TestPriorityDrainsFirstAccount is the core of the requested behavior: the
// earliest account keeps serving until it becomes unusable.
func TestPriorityDrainsFirstAccount(t *testing.T) {
	pool := poolOf(t, "key-a", "key-b", "key-c")
	quota := newStubQuota()
	pool.quota = quota
	pool.strategy = StrategyPriority

	first := accountID("key-a")
	for i := 0; i < 5; i++ {
		got := pool.Acquire()
		if got == nil {
			t.Fatalf("Acquire #%d returned nil", i)
		}
		if got.ID != first {
			t.Fatalf("Acquire #%d = %s, want the first account %s", i, got.ID, first)
		}
	}

	// Exhaust it; the pool must move to the second account.
	quota.set(first, true)
	got := pool.Acquire()
	if got == nil || got.ID != accountID("key-b") {
		t.Fatalf("after exhausting the first account, Acquire = %v, want key-b", got)
	}

	// Exhaust the second too; the third serves.
	quota.set(accountID("key-b"), true)
	got = pool.Acquire()
	if got == nil || got.ID != accountID("key-c") {
		t.Fatalf("after exhausting two accounts, Acquire = %v, want key-c", got)
	}

	// All exhausted means nothing is available.
	quota.set(accountID("key-c"), true)
	if got := pool.Acquire(); got != nil {
		t.Fatalf("Acquire with every account exhausted = %v, want nil", got)
	}
	if !pool.AllExhausted() {
		t.Fatal("AllExhausted should report true")
	}
}

// TestRoundRobinRemainsTheDefault guards the existing behavior: without an
// explicit strategy the pool still spreads load evenly.
func TestRoundRobinRemainsTheDefault(t *testing.T) {
	pool := poolOf(t, "key-a", "key-b")
	if pool.Strategy() != StrategyRoundRobin {
		t.Fatalf("default strategy = %v, want round_robin", pool.Strategy())
	}
	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		acct := pool.Acquire()
		if acct == nil {
			t.Fatal("Acquire returned nil")
		}
		seen[acct.ID]++
	}
	if seen[accountID("key-a")] != 2 || seen[accountID("key-b")] != 2 {
		t.Fatalf("round-robin distribution = %v, want 2/2", seen)
	}
}

// TestQuotaExhaustionSkipsInRoundRobin checks the skip applies to both
// strategies, not just priority.
func TestQuotaExhaustionSkipsInRoundRobin(t *testing.T) {
	pool := poolOf(t, "key-a", "key-b")
	pool.quota = newStubQuota(accountID("key-a"))

	for i := 0; i < 4; i++ {
		acct := pool.Acquire()
		if acct == nil {
			t.Fatal("Acquire returned nil")
		}
		if acct.ID != accountID("key-b") {
			t.Fatalf("Acquire = %s, want only the unexhausted key-b", acct.ID)
		}
	}
}

// TestMissingQuotaDataKeepsAccountInRotation is important: absence of quota
// data must never bench an account, or a fresh deployment with no successful
// quota fetch yet would serve nothing.
func TestMissingQuotaDataKeepsAccountInRotation(t *testing.T) {
	pool := poolOf(t, "key-a")
	pool.quota = newStubQuota() // no data at all
	pool.strategy = StrategyPriority

	if got := pool.Acquire(); got == nil {
		t.Fatal("an account with no quota snapshot must remain selectable")
	}

	if pool.AllExhausted() {
		t.Fatal("no data must not report AllExhausted")
	}
}

// TestAccountBlockedIgnoresFailedSnapshot pins the rule that a quota query
// failure (unknown state) does not remove an account from rotation, while a
// successful snapshot reporting an exceeded window does.
func TestAccountBlockedIgnoresFailedSnapshot(t *testing.T) {
	usage := &UsageTracker{}
	usage.SetQuota("a1", &QuotaSnapshot{
		LastError: "network unreachable",
		Exceeded:  "monthly",
	})
	if usage.AccountBlocked("a1") {
		t.Fatal("a snapshot from a failed query must not block the account")
	}

	usage.SetQuota("a2", &QuotaSnapshot{Exceeded: "monthly"})
	if !usage.AccountBlocked("a2") {
		t.Fatal("an explicitly exceeded account must be blocked")
	}

	usage.SetQuota("a3", &QuotaSnapshot{FiveHour: &QuotaWindow{Exceeded: true}})
	if !usage.AccountBlocked("a3") {
		t.Fatal("an exceeded 5-hour window must block the account")
	}

	// Healthy snapshot: not blocked.
	usage.SetQuota("a4", &QuotaSnapshot{MonthlyCredits: floatPtr(10)})
	if usage.AccountBlocked("a4") {
		t.Fatal("a healthy account must not be blocked")
	}

	// Unknown id: not blocked.
	if usage.AccountBlocked("nope") {
		t.Fatal("an unknown account must not be blocked")
	}
}

// TestPriorityFailsOverOn429 exercises the full path the user asked about: a
// rate-limited first account must hand off to the next one, and the failure
// must be recorded against the account, not the pool.
func TestPriorityFailsOverOn429(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		seen = append(seen, key)
		mu.Unlock()
		if key == "key-a" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"slow down","rateLimit":{"reset":` +
				strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + `}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"finish\",\"finishReason\":\"stop\",\"usage\":{\"promptTokens\":1,\"completionTokens\":1}}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	pool := NewAccountPool([]AccountConfig{
		{Name: "a", APIKey: "key-a"},
		{Name: "b", APIKey: "key-b"},
	}, WithStrategy(StrategyPriority))
	client := NewCCClientWithPool(pool, upstream.URL)

	resp, acct, err := client.Send(t.Context(), &ChatRequest{
		Model:    "m",
		Messages: []Message{{Role: "user", Content: TextContent("hi")}},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	defer resp.Body.Close()

	if acct == nil || acct.ID != accountID("key-b") {
		t.Fatalf("served by %v, want the second account after the 429", acct)
	}
	// key-a was tried once and rejected, then key-b served.
	mu.Lock()
	got := append([]string(nil), seen...)
	mu.Unlock()
	if len(got) != 2 || got[0] != "key-a" || got[1] != "key-b" {
		t.Fatalf("upstream saw %v, want [key-a key-b]", got)
	}

	// The cooled-down account is now skipped, so the next request goes straight
	// to key-b without re-trying key-a.
	mu.Lock()
	seen = nil
	mu.Unlock()
	resp2, acct2, err := client.Send(t.Context(), &ChatRequest{
		Model:    "m",
		Messages: []Message{{Role: "user", Content: TextContent("hi again")}},
	})
	if err != nil {
		t.Fatalf("second Send: %v", err)
	}
	defer resp2.Body.Close()
	if acct2 == nil || acct2.ID != accountID("key-b") {
		t.Fatalf("second request served by %v, want key-b", acct2)
	}
}

// TestPriorityStrategyParsing covers the config/env mapping.
func TestPriorityStrategyParsing(t *testing.T) {
	cases := map[string]SelectionStrategy{
		"":            StrategyRoundRobin,
		"round_robin": StrategyRoundRobin,
		"nonsense":    StrategyRoundRobin,
		"priority":    StrategyPriority,
		"PRIORITY":    StrategyPriority,
		" sequential": StrategyPriority,
	}
	for in, want := range cases {
		if got := parseSelectionStrategy(in); got != want {
			t.Errorf("parseSelectionStrategy(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestAdminSettingsSwitchesStrategy checks the WebUI can flip the strategy at
// runtime and that the pool follows immediately.
func TestAdminSettingsSwitchesStrategy(t *testing.T) {
	srv, pool, _, cfg, _, _ := newAdminTestEnv(t)

	if pool.Strategy() != StrategyRoundRobin {
		t.Fatalf("initial strategy = %v, want round_robin", pool.Strategy())
	}

	resp, payload := adminRequest(t, srv, "PUT", "/admin/api/settings", "admin-pass-123",
		map[string]any{"account_strategy": "priority"})
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d: %v", resp.StatusCode, payload)
	}
	if got := pool.Strategy(); got != StrategyPriority {
		t.Fatalf("pool strategy = %v, want priority", got)
	}
	if got := cfg.SelectionStrategy(); got != StrategyPriority {
		t.Fatalf("config strategy = %v, want priority", got)
	}

	// The response reports the canonical value.
	settings, _ := payload["settings"].(map[string]any)
	if settings == nil || settings["account_strategy"] != "priority" {
		t.Fatalf("settings payload = %v", payload["settings"])
	}

	// An unrecognised value falls back to round-robin rather than erroring.
	resp, _ = adminRequest(t, srv, "PUT", "/admin/api/settings", "admin-pass-123",
		map[string]any{"account_strategy": "bogus"})
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 for an unknown strategy", resp.StatusCode)
	}
	if got := pool.Strategy(); got != StrategyRoundRobin {
		t.Fatalf("pool strategy = %v, want the round_robin fallback", got)
	}
}
