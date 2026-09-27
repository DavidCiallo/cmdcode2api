package app

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// defaultRateLimitCooldown is applied when a 429 carries no usable Retry-After.
const defaultRateLimitCooldown = time.Minute

// Account is one upstream Command Code credential plus its ephemeral health
// state. Durable request/token counters live in the UsageTracker keyed by ID;
// everything here resets on restart.
type Account struct {
	ID      string
	Name    string
	APIKey  string
	Enabled bool
	Errors  atomic.Int64

	mu               sync.Mutex
	lastError        string
	lastErrorAt      time.Time
	lastUsedAt       time.Time
	rateLimitedUntil time.Time
	authFailures     int
}

func newAccount(name, apiKey string, enabled bool) *Account {
	return &Account{ID: accountID(apiKey), Name: name, APIKey: apiKey, Enabled: enabled}
}

// accountID derives a stable identifier from the key so stats survive renames
// and the raw key never has to appear in persisted data or URLs.
func accountID(apiKey string) string {
	sum := sha256.Sum256([]byte(apiKey))
	return "a" + hex.EncodeToString(sum[:4])
}

func (a *Account) MaskedKey() string {
	if len(a.APIKey) > 12 {
		return a.APIKey[:6] + "…" + a.APIKey[len(a.APIKey)-4:]
	}
	return strings.Repeat("*", len(a.APIKey))
}

func (a *Account) RateLimited(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return now.Before(a.rateLimitedUntil)
}

func (a *Account) RecordSuccess() {
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastUsedAt = now
	a.lastError = ""
	a.lastErrorAt = time.Time{}
	a.authFailures = 0
}

func (a *Account) RecordFailure(err error) {
	now := time.Now()
	a.Errors.Add(1)

	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastError = err.Error()
	a.lastErrorAt = now

	var upstreamErr *upstreamAPIError
	if errors.As(err, &upstreamErr) {
		switch {
		case upstreamErr.Status == http.StatusTooManyRequests:
			cooldown := defaultRateLimitCooldown
			if seconds, parseErr := strconv.Atoi(upstreamErr.RetryAfter); parseErr == nil && seconds > 0 {
				cooldown = time.Duration(seconds) * time.Second
			}
			a.rateLimitedUntil = now.Add(cooldown)
		case upstreamErr.Status == http.StatusUnauthorized || upstreamErr.Status == http.StatusForbidden:
			a.authFailures++
		}
	}
}

// authErrorThreshold marks an account as suspect once this many consecutive
// 401/403 failures accumulate. The account stays enabled; the UI surfaces it.
const authErrorThreshold = 3

// AccountView is the JSON projection of an account for the admin API. It never
// contains the raw key.
type AccountView struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	KeyMasked        string     `json:"key_masked"`
	Enabled          bool       `json:"enabled"`
	RateLimited      bool       `json:"rate_limited"`
	Status           string     `json:"status"`
	Errors           int64      `json:"errors"`
	AuthFailures     int        `json:"auth_failures"`
	LastError        string     `json:"last_error,omitempty"`
	LastErrorAt      *time.Time `json:"last_error_at,omitempty"`
	LastUsedAt       *time.Time `json:"last_used_at,omitempty"`
	RateLimitedUntil *time.Time `json:"rate_limited_until,omitempty"`
}

func (a *Account) View() AccountView {
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()

	view := AccountView{
		ID:           a.ID,
		Name:         a.Name,
		KeyMasked:    a.MaskedKey(),
		Enabled:      a.Enabled,
		Errors:       a.Errors.Load(),
		AuthFailures: a.authFailures,
	}
	if a.lastError != "" {
		view.LastError = a.lastError
		lastErrorAt := a.lastErrorAt
		view.LastErrorAt = &lastErrorAt
	}
	if !a.lastUsedAt.IsZero() {
		lastUsedAt := a.lastUsedAt
		view.LastUsedAt = &lastUsedAt
	}
	if now.Before(a.rateLimitedUntil) {
		rateLimitedUntil := a.rateLimitedUntil
		view.RateLimitedUntil = &rateLimitedUntil
		view.RateLimited = true
	}
	switch {
	case !a.Enabled:
		view.Status = "disabled"
	case now.Before(a.rateLimitedUntil):
		view.Status = "rate_limited"
	case a.authFailures >= authErrorThreshold:
		view.Status = "auth_error"
	default:
		view.Status = "ok"
	}
	return view
}

// SelectionStrategy decides how Acquire orders eligible accounts.
type SelectionStrategy uint8

const (
	// StrategyRoundRobin rotates evenly across accounts. This is the default:
	// with no quota data it spreads load and rate limits across the pool.
	StrategyRoundRobin SelectionStrategy = iota
	// StrategyPriority always prefers the earliest account in the configured
	// list, moving on only when that account becomes unusable (disabled, rate
	// limited, or reported exhausted by upstream quota). Use this to drain one
	// account before touching the next.
	StrategyPriority
)

// String renders the strategy name, so logs and error messages are readable.
func (s SelectionStrategy) String() string {
	switch s {
	case StrategyPriority:
		return "priority"
	case StrategyRoundRobin:
		return "round_robin"
	default:
		return "unknown"
	}
}

// QuotaSource reports whether an account is currently exhausted. It is
// satisfied by UsageTracker; keeping it an interface avoids a dependency cycle
// and lets tests supply a stub.
type QuotaSource interface {
	AccountBlocked(id string) bool
}

// AccountPool holds the configured upstream accounts and rotates between them.
type AccountPool struct {
	mu       sync.RWMutex
	accounts []*Account
	cursor   atomic.Uint64

	// strategy and quota are set once during construction (via the options)
	// and read on every Acquire.
	strategy SelectionStrategy
	quota    QuotaSource
}

// PoolOption customises a pool at construction time.
type PoolOption func(*AccountPool)

// WithStrategy selects how Acquire orders eligible accounts.
func WithStrategy(s SelectionStrategy) PoolOption {
	return func(p *AccountPool) { p.strategy = s }
}

// WithQuotaSource supplies the quota cache Acquire consults to skip accounts
// that upstream reports as exhausted.
func WithQuotaSource(q QuotaSource) PoolOption {
	return func(p *AccountPool) { p.quota = q }
}

func NewAccountPool(list []AccountConfig, opts ...PoolOption) *AccountPool {
	pool := &AccountPool{}
	for _, opt := range opts {
		if opt != nil {
			opt(pool)
		}
	}
	for _, ac := range list {
		if strings.TrimSpace(ac.APIKey) == "" {
			continue
		}
		pool.accounts = append(pool.accounts, newAccount(ac.Name, ac.APIKey, ac.IsEnabled()))
	}
	return pool
}

// Strategy reports the pool's selection strategy.
func (p *AccountPool) Strategy() SelectionStrategy {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.strategy
}

// SetStrategy changes the selection strategy at runtime. It takes effect on the
// next Acquire, so the WebUI can switch between spreading load and draining one
// account without a restart.
func (p *AccountPool) SetStrategy(s SelectionStrategy) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.strategy = s
}

// eligible reports whether an account can serve a request right now.
// exhausted is consulted separately so callers can distinguish "busy" from
// "out of credits" when reporting why nothing was available.
func (p *AccountPool) eligible(a *Account, now time.Time) bool {
	return a.Enabled && !a.RateLimited(now)
}

// exhausted reports whether quota data says an account is out of credits. A
// missing snapshot is never treated as exhausted: absence of data must not take
// an account out of rotation.
func (p *AccountPool) exhausted(a *Account) bool {
	if p.quota == nil {
		return false
	}
	return p.quota.AccountBlocked(a.ID)
}

// Acquire picks the next eligible account.
//
// Round-robin advances an atomic cursor and skips disabled, rate-limited, and
// exhausted accounts. Priority walks the configured list in order and returns
// the first usable account, so its earliest entry is drained before later ones
// are touched.
//
// nil means no account can serve right now: either the pool is empty, or every
// enabled account is rate limited or exhausted.
func (p *AccountPool) Acquire() *Account {
	now := time.Now()
	p.mu.RLock()
	defer p.mu.RUnlock()

	n := len(p.accounts)
	if n == 0 {
		return nil
	}

	if p.strategy == StrategyPriority {
		for i := 0; i < n; i++ {
			a := p.accounts[i]
			if !p.eligible(a, now) || p.exhausted(a) {
				continue
			}
			return a
		}
		return nil
	}

	start := int((p.cursor.Add(1) - 1) % uint64(n))
	for i := 0; i < n; i++ {
		a := p.accounts[(start+i)%n]
		if !p.eligible(a, now) || p.exhausted(a) {
			continue
		}
		return a
	}
	return nil
}

func (p *AccountPool) Len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.accounts)
}

// List returns a snapshot of the pool's accounts for background jobs that
// iterate without holding the pool lock.
func (p *AccountPool) List() []*Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]*Account(nil), p.accounts...)
}

func (p *AccountPool) EnabledCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := 0
	for _, a := range p.accounts {
		if a.Enabled {
			n++
		}
	}
	return n
}

// Primary returns the first enabled account, for callers that need a single
// representative key (e.g. the model catalog fetch).
func (p *AccountPool) Primary() *Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, a := range p.accounts {
		if a.Enabled {
			return a
		}
	}
	if len(p.accounts) > 0 {
		return p.accounts[0]
	}
	return nil
}

func (p *AccountPool) Get(id string) *Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, a := range p.accounts {
		if a.ID == id {
			return a
		}
	}
	return nil
}

var errDuplicateAccount = errors.New("an account with this key already exists")

func (p *AccountPool) Add(name, apiKey string, enabled bool) (*Account, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, fmt.Errorf("api_key is required")
	}
	if id := accountID(apiKey); p.Get(id) != nil {
		return nil, errDuplicateAccount
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	account := newAccount(name, apiKey, enabled)
	p.accounts = append(p.accounts, account)
	return account, nil
}

func (p *AccountPool) Remove(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, a := range p.accounts {
		if a.ID == id {
			p.accounts = append(p.accounts[:i], p.accounts[i+1:]...)
			return true
		}
	}
	return false
}

func (p *AccountPool) SetEnabled(id string, enabled bool) bool {
	a := p.Get(id)
	if a == nil {
		return false
	}
	a.Enabled = enabled
	return true
}

func (p *AccountPool) Rename(id, name string) bool {
	a := p.Get(id)
	if a == nil {
		return false
	}
	a.Name = name
	return true
}

// SetKey replaces an account's credential. Because IDs derive from the key,
// the account's ID changes too; the caller is responsible for migrating usage
// counters (UsageTracker.MoveAccount).
func (p *AccountPool) SetKey(id, newKey string) (string, error) {
	newKey = strings.TrimSpace(newKey)
	if newKey == "" {
		return "", fmt.Errorf("api_key cannot be empty")
	}
	newID := accountID(newKey)

	p.mu.Lock()
	defer p.mu.Unlock()
	var a *Account
	for _, cand := range p.accounts {
		if cand.ID == id {
			a = cand
			break
		}
	}
	if a == nil {
		return "", fmt.Errorf("account not found")
	}
	for _, other := range p.accounts {
		if other != a && other.ID == newID {
			return "", errDuplicateAccount
		}
	}
	a.mu.Lock()
	a.APIKey = newKey
	a.ID = newID
	a.mu.Unlock()
	return newID, nil
}

// AllExhausted reports whether every enabled account is currently blocked by
// quota data. Used only to describe why nothing was available, so an empty pool
// reports false.
func (p *AccountPool) AllExhausted() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	enabled := 0
	for _, a := range p.accounts {
		if !a.Enabled {
			continue
		}
		enabled++
		if !p.exhausted(a) {
			return false
		}
	}
	return enabled > 0
}

// EarliestRateLimitWait reports how long until the first rate-limited account
// recovers, for the 429 response sent when every enabled account is cooling
// down.
func (p *AccountPool) EarliestRateLimitWait(now time.Time) time.Duration {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var earliest time.Time
	for _, a := range p.accounts {
		if !a.Enabled {
			continue
		}
		a.mu.Lock()
		until := a.rateLimitedUntil
		a.mu.Unlock()
		if until.After(now) && (earliest.IsZero() || until.Before(earliest)) {
			earliest = until
		}
	}
	if earliest.IsZero() {
		return 0
	}
	return earliest.Sub(now)
}

func (p *AccountPool) Views() []AccountView {
	p.mu.RLock()
	defer p.mu.RUnlock()
	views := make([]AccountView, 0, len(p.accounts))
	for _, a := range p.accounts {
		views = append(views, a.View())
	}
	return views
}

// Stats returns total, enabled, and currently-rate-limited account counts.
func (p *AccountPool) Stats() (total, enabled, rateLimited int) {
	now := time.Now()
	p.mu.RLock()
	defer p.mu.RUnlock()
	total = len(p.accounts)
	for _, a := range p.accounts {
		if a.Enabled {
			enabled++
			if a.RateLimited(now) {
				rateLimited++
			}
		}
	}
	return total, enabled, rateLimited
}

// Config projects the pool back into its YAML representation for persistence.
func (p *AccountPool) Config() []AccountConfig {
	p.mu.RLock()
	defer p.mu.RUnlock()
	list := make([]AccountConfig, 0, len(p.accounts))
	for _, a := range p.accounts {
		enabled := a.Enabled
		list = append(list, AccountConfig{
			Name:    a.Name,
			APIKey:  a.APIKey,
			Enabled: &enabled,
		})
	}
	return list
}

// SyncToConfig copies the pool state into cfg so a subsequent saveConfig
// persists it. The legacy single-key field is cleared whenever accounts exist
// so a deleted account cannot resurrect from the stale field.
func (p *AccountPool) SyncToConfig(cfg *Config) {
	cfg.CommandCode.Accounts = p.Config()
	if len(cfg.CommandCode.Accounts) > 0 {
		cfg.CommandCode.APIKey = ""
	}
}
