package app

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	oauthPortStart = 5959
	oauthPortRange = 10
	studioBaseURL  = "https://commandcode.ai"
	oauthTimeout   = 10 * time.Minute
)

type oauthCallback struct {
	APIKey   string `json:"apiKey"`
	State    string `json:"state"`
	UserID   string `json:"userId"`
	UserName string `json:"userName"`
	KeyName  string `json:"keyName"`
}

type OAuthOptions struct {
	CallbackURL string
	// NoLocalListener skips the 127.0.0.1 callback server. Use when the
	// callback is delivered through another endpoint (e.g. the WebUI's own
	// /admin/api/oauth/callback), which then must call flow.deliver.
	NoLocalListener bool
}

// OAuthFlow is a running OAuth authorization. The CLI blocks on Wait; the
// WebUI polls State until it leaves "pending". All methods are safe for
// concurrent use.
type OAuthFlow struct {
	AuthURL     string
	CallbackURL string
	Port        int
	State       string

	server     *http.Server // nil in NoLocalListener mode
	noListener bool
	resultCh   chan oauthCallback
	errCh      chan error
	done       chan struct{}
	closeOnce  sync.Once

	mu     sync.Mutex
	result *oauthCallback
	err    error
}

// generateState 生成随机 state token 防 CSRF
func generateState() (string, error) {
	state, err := randomHex(32)
	if err != nil {
		return "", fmt.Errorf("generate oauth state: %w", err)
	}
	return base64.URLEncoding.EncodeToString([]byte(state)), nil
}

// StartOAuthFlow starts the OAuth flow and returns a handle for awaiting the
// authorization result. Unless NoLocalListener is set, it also starts the
// 127.0.0.1 callback server; the caller must eventually Wait or Cancel the
// flow so the listener is released.
func StartOAuthFlow(opts OAuthOptions) (*OAuthFlow, error) {
	if opts.CallbackURL != "" {
		if err := validateCallbackURL(opts.CallbackURL); err != nil {
			return nil, err
		}
	}

	if opts.NoLocalListener {
		return newOAuthFlow(opts, nil)
	}

	listenHost := "127.0.0.1"
	listenPort := oauthPortStart

	listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", listenHost, listenPort))
	if err != nil {
		// 尝试下一个端口
		for port := oauthPortStart + 1; port < oauthPortStart+oauthPortRange; port++ {
			listener, err = net.Listen("tcp", fmt.Sprintf("%s:%d", listenHost, port))
			if err == nil {
				listenPort = port
				break
			}
		}
		if err != nil {
			return nil, fmt.Errorf("start callback server: %w", err)
		}
	}

	resultCh := make(chan oauthCallback, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		// CORS
		w.Header().Set("Access-Control-Allow-Origin", "https://commandcode.ai")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Private-Network", "true")

		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}

		if r.Method != "POST" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(405)
			json.NewEncoder(w).Encode(map[string]any{
				"success": false,
				"error":   "method not allowed",
			})
			return
		}

		var cb oauthCallback
		if err := json.NewDecoder(r.Body).Decode(&cb); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]any{
				"success": false,
				"error":   "invalid JSON",
			})
			return
		}

		// 错误回调
		if errMsg, _ := r.URL.Query()["error"]; len(errMsg) > 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]any{"success": true})
			errCh <- fmt.Errorf("authorization canceled: %s", errMsg[0])
			return
		}

		if cb.APIKey == "" || cb.State == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]any{
				"success": false,
				"error":   "api_key and state are required",
			})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{"success": true})

		resultCh <- cb
	})

	server := &http.Server{Handler: mux}
	go func() {
		_ = server.Serve(listener) // server 被关闭是正常的
	}()

	flow, err := newOAuthFlow(opts, server)
	if err != nil {
		server.Close()
		return nil, err
	}
	flow.Port = listener.Addr().(*net.TCPAddr).Port
	flow.CallbackURL = opts.CallbackURL
	if flow.CallbackURL == "" {
		flow.CallbackURL = fmt.Sprintf("http://localhost:%d/callback", flow.Port)
	}
	flow.AuthURL = buildAuthURL(flow.CallbackURL, flow.State)
	if err := flow.writeScratchFiles(); err != nil {
		flow.finish(err)
		return nil, err
	}
	return flow, nil
}

// newOAuthFlow builds the flow shared state. The listener server is optional
// (NoLocalListener mode delivers via flow.deliver instead).
func newOAuthFlow(opts OAuthOptions, server *http.Server) (*OAuthFlow, error) {
	state, err := generateState()
	if err != nil {
		return nil, err
	}
	flow := &OAuthFlow{
		State:      state,
		server:     server,
		resultCh:   make(chan oauthCallback, 1),
		errCh:      make(chan error, 1),
		done:       make(chan struct{}),
		noListener: opts.NoLocalListener,
	}
	if opts.CallbackURL != "" {
		flow.CallbackURL = opts.CallbackURL
		flow.AuthURL = buildAuthURL(opts.CallbackURL, state)
		if writeErr := flow.writeScratchFiles(); writeErr != nil {
			return nil, writeErr
		}
	}
	return flow, nil
}

func buildAuthURL(callbackURL, state string) string {
	return fmt.Sprintf("%s/studio/auth/cli?callback=%s&state=%s",
		studioBaseURL, url.QueryEscape(callbackURL), state)
}

// writeScratchFiles keeps the URL/state in memory for the lifetime of the
// process. There is no data volume, so nothing is written to disk; background
// CLI runs read the values from the flow struct directly.
func (f *OAuthFlow) writeScratchFiles() error {
	return nil
}

// deliver accepts a callback payload from an external transport (the WebUI
// callback endpoint). It mirrors the state check the local server relies on.
func (f *OAuthFlow) deliver(cb oauthCallback) error {
	select {
	case <-f.done:
		return fmt.Errorf("OAuth flow is no longer pending")
	default:
	}
	if cb.State != f.State {
		return fmt.Errorf("state token mismatch (possible tampering)")
	}
	f.mu.Lock()
	f.result = &cb
	f.mu.Unlock()
	f.finish(nil)
	select {
	case f.resultCh <- cb:
	default:
	}
	return nil
}

// Wait blocks until the flow succeeds, fails, or times out.
func (f *OAuthFlow) Wait(timeout time.Duration) (oauthCallback, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case cb := <-f.resultCh:
		if cb.State != f.State {
			err := fmt.Errorf("state token mismatch (possible tampering)")
			f.finish(err)
			return oauthCallback{}, err
		}
		f.mu.Lock()
		f.result = &cb
		f.mu.Unlock()
		f.finish(nil)
		return cb, nil
	case err := <-f.errCh:
		f.finish(err)
		return oauthCallback{}, err
	case <-timer.C:
		err := fmt.Errorf("OAuth timed out after %s", timeout)
		f.finish(err)
		return oauthCallback{}, err
	}
}

// Cancel aborts a pending flow. It is safe to call at any time.
func (f *OAuthFlow) Cancel() {
	select {
	case f.errCh <- fmt.Errorf("OAuth flow canceled"):
	default:
	}
	f.finish(nil)
}

// finish marks the flow complete exactly once and releases the listener.
func (f *OAuthFlow) finish(err error) {
	if err != nil {
		f.mu.Lock()
		if f.err == nil {
			f.err = err
		}
		f.mu.Unlock()
	}
	f.closeOnce.Do(func() {
		close(f.done)
		if f.server != nil {
			_ = f.server.Close()
		}
	})
}

// State returns "pending", "success", or "failed" without blocking.
func (f *OAuthFlow) StateName() string {
	select {
	case <-f.done:
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.err != nil {
			return "failed"
		}
		return "success"
	default:
		return "pending"
	}
}

// Result returns the callback payload once the state is "success".
func (f *OAuthFlow) Result() (oauthCallback, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.result == nil {
		return oauthCallback{}, false
	}
	return *f.result, true
}

// Err returns the failure reason once the state is "failed".
func (f *OAuthFlow) Err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

// displayName picks the human label for an OAuth-authorized account: the
// logged-in user's account name first, then the key's name.
func (cb oauthCallback) displayName() string {
	if name := strings.TrimSpace(cb.UserName); name != "" {
		return name
	}
	return strings.TrimSpace(cb.KeyName)
}

// runOAuth runs the CLI flow: start, print instructions, wait.
func runOAuth(opts OAuthOptions) (oauthCallback, error) {
	flow, err := StartOAuthFlow(opts)
	if err != nil {
		return oauthCallback{}, err
	}

	log.Printf("waiting for Command Code OAuth callback on http://127.0.0.1:%d/callback", flow.Port)

	fmt.Printf(`Command Code OAuth

Open this URL in your browser:
  %s

Callback URL:
  %s

If this is running on a remote server, make sure that callback URL reaches:
  http://127.0.0.1:%d/callback

Waiting for authorization, timeout: %s

`, flow.AuthURL, flow.CallbackURL, flow.Port, oauthTimeout)

	cb, err := flow.Wait(oauthTimeout)
	if err != nil {
		return oauthCallback{}, err
	}
	log.Printf("✓ OAuth success: user %s, key %s", cb.UserName, cb.KeyName)
	return cb, nil
}

// ParseCallbackURL reads a callback payload out of the URL the browser was
// redirected to. Command Code's redirect mode delivers the credential as
// query parameters (?apiKey=…&state=…&userId=…&userName=…&keyName=…), which is
// what a user copies out of the address bar when their browser cannot reach
// the server's 127.0.0.1 listener. The payload is extracted locally: the
// gateway never fetches the pasted URL, so an attacker-supplied link cannot
// turn this into a request forgery, and the state check below still applies.
//
// Messages are English sources so i18n.Message can localize them; this
// function has no request context of its own.
func ParseCallbackURL(rawURL string) (oauthCallback, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return oauthCallback{}, fmt.Errorf("callback link is required")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return oauthCallback{}, fmt.Errorf("parse callback link: %w", err)
	}
	q := parsed.Query()

	// The auth URL is the page that leads to the callback; pasting it by
	// mistake is common, so name the problem precisely.
	if q.Get("callback") != "" && q.Get("apiKey") == "" {
		return oauthCallback{}, fmt.Errorf("that is the authorization link, not the callback link: finish authorizing in the browser, then copy the link from the address bar")
	}
	if errMsg := q.Get("error"); errMsg != "" {
		desc := q.Get("error_description")
		if desc == "" {
			desc = errMsg
		}
		return oauthCallback{}, fmt.Errorf("authorization denied: %s", desc)
	}

	cb := oauthCallback{
		APIKey:   q.Get("apiKey"),
		State:    q.Get("state"),
		UserID:   q.Get("userId"),
		UserName: q.Get("userName"),
		KeyName:  q.Get("keyName"),
	}
	if cb.APIKey == "" || cb.State == "" {
		return oauthCallback{}, fmt.Errorf("the link is missing apiKey / state: copy the full link from the address bar after the redirect")
	}
	return cb, nil
}

func validateCallbackURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse oauth callback url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("oauth callback url must use http or https")
	}
	if parsed.Hostname() == "" {
		return fmt.Errorf("oauth callback url must include a host")
	}
	if !strings.HasSuffix(parsed.Path, "/callback") {
		return fmt.Errorf("oauth callback url path must end with /callback")
	}
	return nil
}
