package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CCClient sends requests to the Command Code upstream, rotating across the
// accounts in its pool and failing over on account-scoped errors.
type CCClient struct {
	// APIKey is only used when Pool is nil (tests / legacy construction).
	APIKey  string
	Pool    *AccountPool
	BaseURL string
	Client  *http.Client

	// baseURLMu guards BaseURL, which the admin API can update at runtime.
	baseURLMu sync.RWMutex
}

func NewCCClient(apiKey, baseURL string) *CCClient {
	return NewCCClientWithPool(NewAccountPool([]AccountConfig{{Name: "default", APIKey: apiKey}}), baseURL)
}

func NewCCClientWithPool(pool *AccountPool, baseURL string) *CCClient {
	return &CCClient{
		Pool:    pool,
		BaseURL: baseURL,
		Client:  &http.Client{Timeout: 600 * time.Second},
	}
}

func (c *CCClient) BaseURLValue() string {
	c.baseURLMu.RLock()
	defer c.baseURLMu.RUnlock()
	return c.BaseURL
}

func (c *CCClient) SetBaseURL(url string) {
	c.baseURLMu.Lock()
	defer c.baseURLMu.Unlock()
	c.BaseURL = url
}

func (c *CCClient) pool() *AccountPool {
	if c.Pool == nil {
		return NewAccountPool([]AccountConfig{{Name: "default", APIKey: c.APIKey}})
	}
	return c.Pool
}

type invalidRequestError struct {
	message string
}

func (e *invalidRequestError) Error() string {
	return e.message
}

type upstreamAPIError struct {
	Status     int
	Message    string
	Type       string
	Code       string
	RetryAfter string
	RequestID  string
}

func (e *upstreamAPIError) Error() string {
	return fmt.Sprintf("cc api error %d: %s", e.Status, e.Message)
}

func normalizeUpstreamError(status int, body []byte, header http.Header) *upstreamAPIError {
	type rateLimitInfo struct {
		Reset float64 `json:"reset"`
	}
	var payload struct {
		Message   string        `json:"message"`
		Type      string        `json:"type"`
		Code      string        `json:"code"`
		RateLimit rateLimitInfo `json:"rateLimit"`
		Error     struct {
			Message   string        `json:"message"`
			Type      string        `json:"type"`
			Code      string        `json:"code"`
			RateLimit rateLimitInfo `json:"rateLimit"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &payload)

	message, code := payload.Message, payload.Code
	reset := payload.RateLimit.Reset
	if payload.Error.Message != "" {
		message = payload.Error.Message
	}
	if payload.Error.Code != "" {
		code = payload.Error.Code
	}
	if payload.Error.RateLimit.Reset > 0 {
		reset = payload.Error.RateLimit.Reset
	}
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	if message == "" {
		message = http.StatusText(status)
	}

	typ, defaultCode := normalizedErrorTypeAndCode(status)
	if code == "" {
		code = defaultCode
	}

	retryAfter := header.Get("Retry-After")
	if status == http.StatusTooManyRequests && retryAfter == "" {
		retryAfter = retryAfterFromRateLimit(reset, message, time.Now())
	}
	requestID := header.Get("x-request-id")
	if requestID == "" {
		requestID = header.Get("lb-request-id")
	}
	return &upstreamAPIError{
		Status:     status,
		Message:    message,
		Type:       typ,
		Code:       code,
		RetryAfter: retryAfter,
		RequestID:  requestID,
	}
}

func normalizedErrorTypeAndCode(status int) (string, string) {
	switch status {
	case http.StatusBadRequest, http.StatusConflict, http.StatusUnprocessableEntity:
		return "invalid_request_error", "invalid_request"
	case http.StatusUnauthorized:
		return "authentication_error", "invalid_api_key"
	case http.StatusForbidden:
		return "permission_error", "permission_denied"
	case http.StatusNotFound:
		return "not_found_error", "not_found"
	case http.StatusTooManyRequests:
		return "rate_limit_error", "rate_limit_exceeded"
	default:
		if status >= http.StatusInternalServerError {
			return "server_error", "upstream_server_error"
		}
		return "api_error", "upstream_api_error"
	}
}

func retryAfterFromRateLimit(reset float64, message string, now time.Time) string {
	var resetAt time.Time
	if reset > 0 {
		seconds, fraction := math.Modf(reset)
		resetAt = time.Unix(int64(seconds), int64(fraction*float64(time.Second)))
	} else {
		lower := strings.ToLower(message)
		const marker = "resets at "
		if index := strings.Index(lower, marker); index >= 0 {
			fields := strings.Fields(message[index+len(marker):])
			if len(fields) > 0 {
				value := strings.Trim(fields[0], ".,;)]}")
				resetAt, _ = time.Parse(time.RFC3339Nano, value)
			}
		}
	}
	if resetAt.IsZero() || !resetAt.After(now) {
		return ""
	}
	seconds := int64((resetAt.Sub(now) + time.Second - 1) / time.Second)
	return strconv.FormatInt(seconds, 10)
}

// Send rotates across the account pool: each attempt uses the next eligible
// account, and account-scoped failures (401/403/429/5xx, transport errors)
// move on to the next one. Request-scoped failures (400/422, canceled
// contexts) return immediately. Failover only happens while the client
// response is still unwritten — once a stream starts, it is never replayed.
// The returned Account is the credential that produced the response or error.
func (c *CCClient) Send(ctx context.Context, req *ChatRequest) (*http.Response, *Account, error) {
	ccReq, err := openAIToCC(req)
	if err != nil {
		return nil, nil, err
	}

	body, err := json.Marshal(ccReq)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal cc request: %w", err)
	}

	pool := c.pool()
	attempts := pool.EnabledCount()
	if attempts == 0 {
		return nil, nil, &upstreamAPIError{
			Status:  http.StatusServiceUnavailable,
			Type:    "server_error",
			Code:    "no_accounts",
			Message: "no enabled Command Code accounts",
		}
	}

	var lastErr error
	var lastAcct *Account
	for attempt := 0; attempt < attempts; attempt++ {
		acct := pool.Acquire()
		if acct == nil {
			// Every enabled account is cooling down from a 429.
			break
		}
		lastAcct = acct
		resp, err := c.doSend(ctx, body, acct.APIKey)
		if err == nil {
			acct.RecordSuccess()
			return resp, acct, nil
		}
		acct.RecordFailure(err)
		if !shouldFailover(err) {
			return nil, acct, err
		}
		lastErr = err
		log.Printf("[WARN] account %s request failed, failing over: %v", acct.Name, err)
	}
	if lastErr != nil {
		return nil, lastAcct, lastErr
	}

	wait := pool.EarliestRateLimitWait(time.Now()).Round(time.Second)
	retryAfter := ""
	message := "no Command Code account is currently available"
	switch {
	case wait > 0:
		retryAfter = strconv.FormatInt(int64(wait.Seconds()), 10)
		message = fmt.Sprintf("all Command Code accounts are rate limited; next account available in %s", wait)
	case pool.AllExhausted():
		// Every enabled account is out of credits according to upstream quota.
		message = "all Command Code accounts are out of quota"
	}
	return nil, nil, &upstreamAPIError{
		Status:     http.StatusTooManyRequests,
		Type:       "rate_limit_error",
		Code:       "rate_limit_exceeded",
		Message:    message,
		RetryAfter: retryAfter,
	}
}

// shouldFailover reports whether an error is worth retrying with a different
// account. Anything that would fail identically on every account (a malformed
// request, an abandoned connection) is not.
func shouldFailover(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var invalid *invalidRequestError
	if errors.As(err, &invalid) {
		return false
	}
	var upstreamErr *upstreamAPIError
	if errors.As(err, &upstreamErr) {
		switch upstreamErr.Status {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
			return true
		}
		return upstreamErr.Status >= http.StatusInternalServerError
	}
	return true
}

// setCCHeaders applies the headers the official Command Code CLI sends. Both
// the generation path and the quota path go through it so the client identity
// stays identical across every upstream call.
func setCCHeaders(h http.Header, apiKey string) {
	h.Set("Authorization", "Bearer "+apiKey)
	h.Set("x-command-code-version", ccCLIVersion)
	h.Set("x-cli-environment", ccCLIEnvironment)
	h.Set("User-Agent", ccUserAgent)
}

func (c *CCClient) doSend(ctx context.Context, body []byte, apiKey string) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.BaseURLValue()+"/alpha/generate", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	setCCHeaders(httpReq.Header, apiKey)

	resp, err := c.Client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		resp.Body.Close()
		return nil, normalizeUpstreamError(resp.StatusCode, body, resp.Header)
	}
	return resp, nil
}

// maxSSELineBytes bounds one SSE line. A tool-call event carries the entire
// tool input inline, so this must clear the largest response the upstream can
// produce: maximumCCMaxTokens is roughly 800 KB of text, and JSON escaping
// inflates that further. Overshooting is reported as an error rather than
// silently truncating the stream.
//
// 4 MB leaves >4x headroom over the ~1 MB worst case above while keeping the
// per-line ceiling low enough that a hostile or broken upstream cannot make a
// single event allocate hundreds of MB. The previous 32 MB allowed one line to
// amplify into ~200 MB of live allocation once parsed.
const maxSSELineBytes = 4 * 1024 * 1024

// maxSSEStreamBytes bounds the total payload of one upstream stream. Per-line
// and per-event caps alone do not bound memory: an upstream may emit an
// unlimited number of individually-legal events (and the authoritative
// tool-call set is retained for the whole stream), so a stream-level ceiling is
// what actually keeps a single request's footprint finite.
const maxSSEStreamBytes = 64 * 1024 * 1024

// readSSELine reads one newline-terminated line, up to maxSSELineBytes.
//
// bufio.Scanner cannot do this: a token larger than its buffer stops the scan
// with ErrTooLong, so a single oversized tool-call event would discard every
// event after it — including the finish event — and the client would see a
// stream that just stops.
//
// The line is assembled in a []byte that grows geometrically from the reader's
// own buffer size. A strings.Builder here re-copied the payload on every
// doubling (56 MB moved to produce an 8 MB line); starting at the reader's
// buffer size and using append keeps the copy count proportional to log(n).
func readSSELine(r *bufio.Reader) (string, error) {
	chunk, err := r.ReadSlice('\n')
	if err == nil {
		// Fast path: the whole line was already sitting in the reader's buffer.
		// ReadSlice's slice is only valid until the next read, and callers keep
		// the line across the callback that may read more, so copy exactly once.
		return string(chunk), nil
	}
	if err != bufio.ErrBufferFull {
		if len(chunk) > maxSSELineBytes {
			return "", fmt.Errorf("sse line exceeds %d bytes", maxSSELineBytes)
		}
		return string(chunk), err
	}

	buf := make([]byte, 0, len(chunk)*2)
	buf = append(buf, chunk...)
	for {
		chunk, err = r.ReadSlice('\n')
		if len(buf)+len(chunk) > maxSSELineBytes {
			return "", fmt.Errorf("sse line exceeds %d bytes", maxSSELineBytes)
		}
		buf = append(buf, chunk...)
		if err == nil {
			return string(buf), nil
		}
		if err != bufio.ErrBufferFull {
			return string(buf), err
		}
	}
}

func decodeSSEEvent(payload string) (CCStreamEvent, error) {
	var ev CCStreamEvent
	decoder := json.NewDecoder(strings.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(&ev); err != nil {
		return ev, fmt.Errorf("parse sse data: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return ev, fmt.Errorf("parse sse data: trailing content: %w", err)
	}
	return ev, nil
}

// ccCLIVersion is reported as x-command-code-version on every upstream request.
// Command Code does not currently reject stale values (nor a missing header),
// but the field identifies the client on their side, so keep it close to the
// command-code release the wire format below was verified against.
// Last verified against command-code 1.54.2 (/alpha/generate, tool-call and
// reasoning events, /alpha/whoami, /alpha/billing/* and /alpha/usage/summary).
const ccCLIVersion = "1.54.2"

// ccCLIEnvironment mirrors the official CLI's x-cli-environment value.
const ccCLIEnvironment = "production"

// ccUserAgent matches the official CLI, which sends the literal "cli".
// The upstream keys its request handling off the CLI headers, so staying
// identical to the real client is safer than advertising ourselves.
const ccUserAgent = "cli"

type streamEndKind uint8

const (
	streamEndEOF streamEndKind = iota + 1
	streamEndDone
)

// ParseStreamEvents reads upstream SSE events. Callers that need to distinguish
// an explicit [DONE] from a bare EOF use parseStreamEvents directly.
func ParseStreamEvents(resp *http.Response, onEvent func(CCStreamEvent) error) error {
	_, err := parseStreamEvents(resp, onEvent)
	return err
}

func parseStreamEvents(resp *http.Response, onEvent func(CCStreamEvent) error) (streamEndKind, error) {
	defer resp.Body.Close()
	reader := bufio.NewReaderSize(resp.Body, 64*1024)
	var dataLines []string
	dataBytes := 0
	streamBytes := 0

	dispatch := func() (bool, error) {
		if len(dataLines) == 0 {
			return false, nil
		}
		var payload string
		if len(dataLines) == 1 {
			payload = dataLines[0]
		} else {
			payload = strings.Join(dataLines, "\n")
		}
		dataLines = nil
		dataBytes = 0
		if strings.TrimSpace(payload) == "" {
			return false, nil
		}
		if strings.TrimSpace(payload) == "[DONE]" {
			return true, nil
		}
		ev, err := decodeSSEEvent(payload)
		if err != nil {
			return false, err
		}
		if err := onEvent(ev); err != nil {
			return false, err
		}
		return false, nil
	}

	for {
		raw, readErr := readSSELine(reader)
		// TrimSuffix/TrimSpace copy the whole string when a trim is needed, so
		// only touch the line when there is actually something to remove.
		line := raw
		if strings.HasSuffix(line, "\r") {
			line = line[:len(line)-1]
		}
		streamBytes += len(line)
		if streamBytes > maxSSEStreamBytes {
			return 0, fmt.Errorf("sse stream exceeds %d bytes", maxSSEStreamBytes)
		}

		switch {
		case line == "":
			done, err := dispatch()
			if err != nil {
				return 0, err
			}
			if done {
				return streamEndDone, nil
			}
		case strings.HasPrefix(line, ":"):
			// SSE comment/keep-alive.
		case strings.HasPrefix(line, "data:"):
			value := line[len("data:"):]
			if strings.HasPrefix(value, " ") {
				value = value[1:]
			}
			dataBytes += len(value)
			if dataBytes > maxSSELineBytes {
				return 0, fmt.Errorf("sse event exceeds %d bytes", maxSSELineBytes)
			}
			dataLines = append(dataLines, value)
		case isSSEFieldLine(line):
			// event, id and retry fields do not carry the JSON payload.
		default:
			// Preserve compatibility with upstreams that send bare JSON lines.
			if len(dataLines) > 0 {
				done, err := dispatch()
				if err != nil {
					return 0, err
				}
				if done {
					return streamEndDone, nil
				}
			}
			dataLines = append(dataLines, strings.TrimSpace(line))
			done, err := dispatch()
			if err != nil {
				return 0, err
			}
			if done {
				return streamEndDone, nil
			}
		}

		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return 0, readErr
			}
			done, err := dispatch()
			if err != nil {
				return 0, err
			}
			if done {
				return streamEndDone, nil
			}
			return streamEndEOF, nil
		}
	}
}

// isSSEFieldLine reports whether a line begins with a known non-data SSE field.
func isSSEFieldLine(line string) bool {
	for _, field := range []string{"event:", "id:", "retry:"} {
		if strings.HasPrefix(line, field) {
			return true
		}
	}
	return false
}

// ====================== 格式转换 ======================

// resolveModelName 将客户端传来的 model ID 映射为 CC API 期望的格式。
// 优先使用动态 modelCatalog（来自 /provider/v1/models），
// 回退到根据模型名推断 provider 前缀。
func resolveModelName(model string) string {
	// 已有 provider 前缀（含 /），直接使用
	if strings.Contains(model, "/") {
		return model
	}

	// 在动态 catalog 中查找匹配的 ID（catalog 中的 ID 已含正确前缀）
	for _, m := range modelCatalog {
		if m.ID == model || strings.HasSuffix(m.ID, "/"+model) {
			return m.ID
		}
	}

	// catalog 中未找到，根据模型名前缀推断 provider
	switch {
	case strings.HasPrefix(model, "gemini-"):
		return "google/" + model
	case strings.HasPrefix(model, "claude-"):
		return "anthropic/" + model
	case strings.HasPrefix(model, "gpt-"):
		return "openai/" + model
	default:
		return model
	}
}

const (
	defaultCCMaxTokens = 64_000
	maximumCCMaxTokens = 200_000
)

func openAIToCC(req *ChatRequest) (CCRequest, error) {
	tools := toolsToCC(req.Tools)
	msgs, err := messagesToCC(req.Messages)
	if err != nil {
		return CCRequest{}, err
	}
	system := extractSystem(req.Messages)

	cc := CCRequest{
		Config: CCConfig{
			WorkingDir:    "/",
			Date:          time.Now().Format("2006-01-02"),
			Environment:   fmt.Sprintf("%s-%s, Go proxy", runtime.GOOS, runtime.GOARCH),
			Structure:     []string{},
			RecentCommits: []any{},
		},
		Memory:         "",
		Taste:          "",
		Skills:         nil,
		PermissionMode: "standard",
		Params: CCParams{
			Model:     resolveModelName(req.Model),
			Messages:  msgs,
			Tools:     tools,
			System:    system,
			MaxTokens: req.OutputTokenBudget(),
			Stream:    true, // CC API 只支持流式
		},
	}
	// command-code's main agent, print mode, and subagents all request 64k
	// output tokens. Match that behavior when OpenAI-compatible clients omit
	// max_tokens so long tool arguments are not cut off at the old 4k default.
	if cc.Params.MaxTokens <= 0 {
		cc.Params.MaxTokens = defaultCCMaxTokens
	}
	if cc.Params.MaxTokens > maximumCCMaxTokens {
		cc.Params.MaxTokens = maximumCCMaxTokens
	}
	return cc, nil
}

func extractSystem(msgs []Message) string {
	var parts []string
	for _, m := range msgs {
		if m.Role == "system" || m.Role == "developer" {
			if text := m.Content.PlainText(); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func messagesToCC(msgs []Message) ([]CCMsg, error) {
	var out []CCMsg
	toolNames := make(map[string]string)
	for _, m := range msgs {
		if m.Role == "system" || m.Role == "developer" {
			continue // 已提取到 top-level system
		}
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				if tc.ID != "" && tc.Function.Name != "" {
					toolNames[tc.ID] = tc.Function.Name
				}
			}
		}
		if m.Role == "tool" && m.Name == "" {
			m.Name = toolNames[m.ToolCallID]
		}
		content, err := contentToCC(m)
		if err != nil {
			return nil, err
		}
		cc := CCMsg{Role: roleToCC(m.Role), Content: content}
		out = append(out, cc)
	}
	return out, nil
}

func roleToCC(role string) string {
	switch role {
	case "assistant", "tool":
		return role
	default:
		return "user"
	}
}

func contentToCC(m Message) ([]CCPart, error) {
	if m.Role == "tool" {
		if m.ToolCallID == "" {
			return nil, &invalidRequestError{message: "tool message requires tool_call_id"}
		}
		if m.Name == "" {
			return nil, &invalidRequestError{message: fmt.Sprintf("cannot resolve tool name for tool_call_id %q", m.ToolCallID)}
		}
		for _, part := range m.Content.PartsValue() {
			if part.Type != "text" {
				return nil, &invalidRequestError{message: fmt.Sprintf("unsupported tool result content type %q", part.Type)}
			}
		}
		return []CCPart{{
			Type:       "tool-result",
			ToolCallID: m.ToolCallID,
			ToolName:   m.Name,
			Output: &CCOutput{
				Type:  "text",
				Value: m.Content.PlainText(),
			},
		}}, nil
	}

	parts := []CCPart{}
	if m.Role == "assistant" && m.ReasoningContent != "" {
		parts = append(parts, CCPart{Type: "reasoning", Text: m.ReasoningContent})
	}
	if text, ok := m.Content.TextValue(); ok && text != "" {
		parts = append(parts, CCPart{Type: "text", Text: text})
	}
	for _, part := range m.Content.PartsValue() {
		switch part.Type {
		case "text":
			if part.Text != "" {
				parts = append(parts, CCPart{Type: "text", Text: part.Text})
			}
		case "image_url":
			if part.ImageURL == nil || part.ImageURL.URL == "" {
				return nil, &invalidRequestError{message: "image_url content requires a non-empty url"}
			}
			mediaType, data, err := parseDataURL(part.ImageURL.URL)
			if err != nil {
				return nil, &invalidRequestError{message: err.Error()}
			}
			parts = append(parts, CCPart{
				Type: "image",
				Source: map[string]any{
					"type":       "base64",
					"media_type": mediaType,
					"data":       data,
				},
			})
		default:
			return nil, &invalidRequestError{message: fmt.Sprintf("unsupported message content type %q", part.Type)}
		}
	}

	for _, tc := range m.ToolCalls {
		if tc.ID == "" {
			return nil, &invalidRequestError{message: "assistant tool call requires a non-empty id"}
		}
		if tc.Function.Name == "" {
			return nil, &invalidRequestError{message: fmt.Sprintf("assistant tool call %q requires a non-empty function name", tc.ID)}
		}
		input, err := validateToolInputObject(tc.Function.Arguments)
		if err != nil {
			return nil, &invalidRequestError{message: fmt.Sprintf("assistant tool call %q has invalid arguments: %v", tc.ID, err)}
		}
		parts = append(parts, CCPart{
			Type:       "tool-call",
			ToolCallID: tc.ID,
			ToolName:   tc.Function.Name,
			Input:      input,
		})
	}

	return parts, nil
}

func validateToolInputObject(arguments string) (json.RawMessage, error) {
	arguments = strings.TrimSpace(arguments)
	if arguments == "" {
		arguments = "{}"
	}
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, fmt.Errorf("arguments must be a JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return nil, fmt.Errorf("trailing content: %w", err)
	}
	return json.RawMessage(arguments), nil
}

func toolsToCC(tools []Tool) []CCTool {
	if tools == nil {
		return []CCTool{}
	}
	out := make([]CCTool, 0, len(tools))
	for _, t := range tools {
		schema := t.Function.Parameters
		if len(schema) == 0 {
			// OpenAI lets a no-argument tool omit parameters, but the upstream
			// expects every tool to carry a schema. Send the canonical empty
			// object so one no-arg tool does not invalidate the whole request.
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, CCTool{
			Type:        "function",
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: schema,
		})
	}
	return out
}

// parseDataURL parses a base64 image data URL. Remote image URLs are rejected
// because the Command Code payload requires inline image data.
func parseDataURL(rawURL string) (string, string, error) {
	if !strings.HasPrefix(rawURL, "data:") {
		return "", "", fmt.Errorf("image_url must be a base64 data URL")
	}
	after := strings.TrimPrefix(rawURL, "data:")
	parts := strings.SplitN(after, ",", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("image_url contains an invalid data URL")
	}
	if !strings.HasSuffix(parts[0], ";base64") {
		return "", "", fmt.Errorf("image_url data URL must use base64 encoding")
	}
	mediaType := strings.TrimSuffix(parts[0], ";base64")
	if !strings.HasPrefix(mediaType, "image/") {
		return "", "", fmt.Errorf("image_url data URL must contain an image media type")
	}
	if _, err := base64.StdEncoding.DecodeString(parts[1]); err != nil {
		if _, rawErr := base64.RawStdEncoding.DecodeString(parts[1]); rawErr != nil {
			return "", "", fmt.Errorf("image_url contains invalid base64 data: %w", err)
		}
	}
	return mediaType, parts[1], nil
}
