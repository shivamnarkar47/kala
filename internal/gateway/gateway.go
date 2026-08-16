// Package gateway ports harness/gateway.py: the SSE streaming client for the
// OpenAI-compatible chat-completions gateway. Pure and self-contained —
// gateway and dialect are the only packages that know the wire protocol.
package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kaal/kaal/internal/config"
	"github.com/kaal/kaal/internal/jsonpy"
	"github.com/kaal/kaal/internal/messages"
)

// EventKind classifies one stream event.
type EventKind int

const (
	EventContent EventKind = iota
	EventReasoning
	EventToolCall
	EventDone
	EventError
)

// StreamEvent is one event on the gateway → loop road: content | reasoning |
// tool_call | done | error.
type StreamEvent struct {
	Kind         EventKind
	Text         string // content / reasoning text, or the error message
	ToolCall     messages.ToolCall
	FinishReason *string // EventDone only
}

// HTTPStatusError is a non-2xx gateway response — the Go analogue of
// Python's urllib HTTPError at the stream boundary.
type HTTPStatusError struct {
	Code   int
	Header http.Header
	Body   []byte
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("gateway HTTP %d", e.Code)
}

// -- transport ---------------------------------------------------------------

// Opener is the transport seam Gateway.stream() calls; tests swap it (the
// analogue of Python's module-level _urlopen).
type Opener interface {
	Do(req *http.Request, timeout time.Duration) (*http.Response, error)
}

var urlOpen Opener

// sleepFn is the retry-backoff seam; tests swap it to record sleeps. Returns
// false when the context is cancelled during the wait.
var sleepFn = func(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

var proxyEnvVars = []string{"http_proxy", "https_proxy", "HTTP_PROXY", "HTTPS_PROXY", "no_proxy", "NO_PROXY"}

// keepaliveEnabled mirrors the Python rule: keep-alive is on by default; off
// with KAAL_NO_KEEPALIVE=1 or any proxy env var.
func keepaliveEnabled() bool {
	if os.Getenv("KAAL_NO_KEEPALIVE") == "1" {
		return false
	}
	for _, v := range proxyEnvVars {
		if os.Getenv(v) != "" {
			return false
		}
	}
	return true
}

// httpClientOpener performs requests through a pooled net/http.Transport.
// Connection reuse is the transport's native behavior (Python's per-thread
// socket juggling disappears); KAAL_NO_KEEPALIVE/proxy envs disable reuse,
// mirroring the Python fallback path.
type httpClientOpener struct {
	tr *http.Transport
}

func newClientOpener() *httpClientOpener {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = config.RequestTimeout * time.Second
	// Pool tuning (P6): bounded per-host concurrency and idle reuse — the
	// Python per-thread socket juggling disappears entirely.
	tr.MaxConnsPerHost = 4
	tr.MaxIdleConns = 8
	tr.MaxIdleConnsPerHost = 4
	tr.IdleConnTimeout = 90 * time.Second
	if !keepaliveEnabled() {
		// Python falls back to plain urllib (no connection reuse) here.
		tr.DisableKeepAlives = true
	}
	return &httpClientOpener{tr: tr}
}

// NewClientOpener builds a fresh pooled transport. Batch workers get their
// own per the plan §4.3 — one transport per worker goroutine, so a worker's
// connection churn never disturbs the others.
func NewClientOpener() Opener { return newClientOpener() }

func (o *httpClientOpener) Do(req *http.Request, _ time.Duration) (*http.Response, error) {
	return (&http.Client{Transport: o.tr}).Do(req)
}

func init() { setTransport() }

// setTransport rebuilds the opener from the environment (the analogue of
// Python's _set_transport; tests call it after flipping env vars).
func setTransport() {
	urlOpen = newClientOpener()
}

// -- request building ----------------------------------------------------------

// ChatBody is the chat-completions request body. Field order mirrors the
// Python dict insertion order; the forbidden fields (tool_choice,
// temperature, stream_options, store) do not exist here and can never be
// sent — this model rejects them.
type ChatBody struct {
	Model     string `json:"model"`
	Messages  []any  `json:"messages"`
	MaxTokens int    `json:"max_tokens"`
	Stream    bool   `json:"stream"`
	Tools     []any  `json:"tools,omitempty"`
}

// BuildBody builds the request body. maxTokens <= 0 selects the harness
// default; tools is omitted entirely when empty.
func BuildBody(modelID string, msgs []any, tools []any, maxTokens int) ChatBody {
	if maxTokens <= 0 {
		maxTokens = config.MaxOutputTokens
	}
	body := ChatBody{Model: modelID, Messages: msgs, MaxTokens: maxTokens, Stream: true}
	if len(tools) > 0 {
		body.Tools = tools
	}
	return body
}

func buildHeaders(apiKey string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+apiKey)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream")
	// Cloudflare WAF (error 1010) blocks urllib's default UA; this value is
	// proven to pass. Do not invent a custom UA.
	h.Set("User-Agent", "python-requests/2.31.0")
	return h
}

// -- SSE parsing ---------------------------------------------------------------

// parseSSELine extracts the payload of a "data:" SSE line; nil for any other
// line.
func parseSSELine(line []byte) *string {
	s := strings.TrimSpace(string(line))
	if !strings.HasPrefix(s, "data:") {
		return nil
	}
	payload := strings.TrimSpace(s[len("data:"):])
	return &payload
}

type toolAccumulator struct {
	id        string
	name      string
	arguments string
}

// mergeToolCalls merges streaming tool-call deltas into per-index
// accumulators.
func mergeToolCalls(acc map[int]*toolAccumulator, items []any) {
	for _, it := range items {
		item, ok := it.(map[string]any)
		if !ok {
			continue
		}
		index := asInt(item["index"], 0)
		entry := acc[index]
		if entry == nil {
			entry = &toolAccumulator{}
			acc[index] = entry
		}
		if id, ok := item["id"].(string); ok && id != "" {
			entry.id = id
		}
		var fn map[string]any
		if f, ok := item["function"].(map[string]any); ok {
			fn = f
		}
		if name, ok := fn["name"].(string); ok && name != "" {
			entry.name = name
		}
		for _, src := range []map[string]any{item, fn} {
			piece, ok := src["arguments"].(string)
			if !ok {
				piece, ok = src["arguments_delta"].(string)
			}
			if ok {
				entry.arguments += piece
			}
		}
	}
}

// emitToolCalls yields accumulated tool calls in ascending index order,
// skipping nameless ones. Returns false when emit aborts.
func emitToolCalls(acc map[int]*toolAccumulator, emit func(StreamEvent) bool) bool {
	indices := make([]int, 0, len(acc))
	for i := range acc {
		indices = append(indices, i)
	}
	sort.Ints(indices)
	for _, i := range indices {
		e := acc[i]
		if e.name == "" {
			continue
		}
		id := e.id
		if id == "" {
			id = fmt.Sprintf("call_%d", i)
		}
		if !emit(StreamEvent{Kind: EventToolCall, ToolCall: messages.ToolCall{ID: id, Name: e.name, Arguments: e.arguments}}) {
			return false
		}
	}
	return true
}

// joinContentParts joins the string parts of a list/dict content delta;
// non-string parts are ignored.
func joinContentParts(content any) string {
	var parts []any
	if list, ok := content.([]any); ok {
		parts = list
	} else if m, ok := content.(map[string]any); ok {
		for _, v := range m {
			parts = append(parts, v)
		}
	}
	var sb strings.Builder
	for _, p := range parts {
		if s, ok := p.(string); ok {
			sb.WriteString(s)
		}
	}
	return sb.String()
}

// asInt coerces a JSON number (json.Number / float64) to int.
func asInt(v any, def int) int {
	switch n := v.(type) {
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
		if f, err := n.Float64(); err == nil {
			return int(f)
		}
	case float64:
		return int(n)
	}
	return def
}

// parseRetryAfter parses a Retry-After header (delta seconds or HTTP-date)
// into a duration; ok=false when absent/unparseable.
func parseRetryAfter(h http.Header) (time.Duration, bool) {
	v := h.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
		if secs < 0 {
			secs = 0
		}
		return time.Duration(secs) * time.Second, true
	}
	retryAt, err := http.ParseTime(v)
	if err != nil {
		return 0, false
	}
	delay := retryAt.Sub(time.Now())
	if delay < 0 {
		delay = 0
	}
	return delay.Truncate(time.Second), true
}

// preview truncates a body for error messages (the Python [:300] preview).
func preview(body []byte) string {
	s := string(body)
	if len(s) <= 300 {
		return s
	}
	return s[:300]
}

// -- Gateway ------------------------------------------------------------------

// Gateway is the SSE streaming client for OpenAI-compatible chat
// completions.
type Gateway struct {
	BaseURL string
	APIKey  string
	Model   string
	// Opener overrides the transport seam (batch workers pass their own
	// per-worker transport); nil = the package default.
	Opener Opener
}

// ModelID returns the configured model id (the loop's Gateway seam).
func (g *Gateway) ModelID() string { return g.Model }

// Warm pre-opens the transport connection so the first Stream skips the
// connect + TLS handshake. No-op in Go: the pooled transport opens the
// connection on first use and reuses it afterwards, so there is nothing to
// pre-open.
func (g *Gateway) Warm() {}

// Stream streams chat completions, returning a channel of events. Retries
// (5xx, network errors, and HTTP 429 rate limits — all only before any event
// was emitted) up to 3 attempts total with 1s/2s/4s backoff; a 429's
// Retry-After header extends the sleep (capped at 60s). Other 4xx errors
// fail immediately. The channel is closed when the stream is done; terminal
// failures arrive as an EventError.
func (g *Gateway) Stream(ctx context.Context, msgs []any, tools []any, maxTokens int) <-chan StreamEvent {
	ch := make(chan StreamEvent, 64)
	go g.stream(ctx, ch, msgs, tools, maxTokens)
	return ch
}

func (g *Gateway) stream(ctx context.Context, ch chan<- StreamEvent, msgs []any, tools []any, maxTokens int) {
	defer close(ch)
	body, err := jsonpy.Marshal(BuildBody(g.Model, msgs, tools, maxTokens))
	if err != nil {
		ch <- StreamEvent{Kind: EventError, Text: "gateway: marshal body: " + err.Error()}
		return
	}
	url := strings.TrimRight(g.BaseURL, "/") + "/chat/completions"
	backoff := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}
	lastError := "unknown error"
	emittedAny := false
	for attempt := 0; attempt < 3; attempt++ {
		retryAfter := time.Duration(0)
		retryAfterValid := false
		err := g.runAttempt(ctx, url, body, func(ev StreamEvent) bool {
			emittedAny = true
			select {
			case ch <- ev:
				return true
			case <-ctx.Done():
				return false
			}
		})
		if err == nil {
			return
		}
		if errors.Is(err, errEmitAbort) || ctx.Err() != nil {
			return // cancelled: no retry, no error event
		}
		switch e := err.(type) {
		case *HTTPStatusError:
			if e.Code >= 400 && e.Code < 500 && e.Code != 429 {
				// Code/key problem — never retry.
				ch <- StreamEvent{Kind: EventError, Text: fmt.Sprintf("gateway HTTP %d: %s", e.Code, preview(e.Body))}
				return
			}
			lastError = fmt.Sprintf("HTTP %d: %s", e.Code, preview(e.Body))
			if e.Code == 429 {
				retryAfter, retryAfterValid = parseRetryAfter(e.Header)
			}
		default:
			lastError = err.Error()
		}
		if emittedAny {
			// Never retry after visible content.
			ch <- StreamEvent{Kind: EventError, Text: "gateway stream interrupted after content: " + lastError}
			return
		}
		if attempt < 2 {
			sleep := backoff[attempt]
			if retryAfterValid && retryAfter > sleep {
				sleep = retryAfter // max(backoff, retry-after), as Python does
			}
			if sleep > 60*time.Second {
				sleep = 60 * time.Second
			}
			if !sleepFn(ctx, sleep) {
				return
			}
		}
	}
	ch <- StreamEvent{Kind: EventError, Text: "gateway request failed after 3 attempts: " + lastError}
}

var errEmitAbort = errors.New("emit aborted")

// runAttempt runs one full open+read cycle, calling emit for each event.
// Returns nil on a clean [DONE]; any transport/HTTP failure is returned so
// the retry loop can classify it.
func (g *Gateway) runAttempt(ctx context.Context, url string, body []byte, emit func(StreamEvent) bool) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header = buildHeaders(g.APIKey)
	req.GetBody = func() (io.ReadCloser, error) { // lets the transport retry stale pooled connections
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	op := urlOpen
	if g.Opener != nil {
		op = g.Opener
	}
	resp, err := op.Do(req, config.RequestTimeout*time.Second)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return &HTTPStatusError{Code: resp.StatusCode, Header: resp.Header, Body: bodyBytes}
	}
	toolAcc := map[int]*toolAccumulator{}
	var lastFinishReason *string
	reader := bufio.NewReader(resp.Body)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			payload := parseSSELine(line)
			if payload != nil {
				if *payload == "[DONE]" {
					if !emitToolCalls(toolAcc, emit) {
						return errEmitAbort
					}
					if !emit(StreamEvent{Kind: EventDone, FinishReason: lastFinishReason}) {
						return errEmitAbort
					}
					return nil
				}
				if !handleChunk(*payload, toolAcc, &lastFinishReason, emit) {
					return errEmitAbort
				}
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				// Server closed the stream without [DONE].
				if !emitToolCalls(toolAcc, emit) {
					return errEmitAbort
				}
				if !emit(StreamEvent{Kind: EventDone, FinishReason: lastFinishReason}) {
					return errEmitAbort
				}
				return nil
			}
			return readErr
		}
	}
}

// handleChunk parses one SSE data payload; malformed noise is tolerated
// (returns true). Returns false when emit aborts.
func handleChunk(payload string, toolAcc map[int]*toolAccumulator, lastFinishReason **string, emit func(StreamEvent) bool) bool {
	dec := json.NewDecoder(strings.NewReader(payload))
	dec.UseNumber()
	var chunk map[string]any
	if err := dec.Decode(&chunk); err != nil {
		return true // tolerate malformed heartbeat/noise lines
	}
	choices, ok := chunk["choices"].([]any)
	if !ok || len(choices) == 0 {
		return true // some servers send empty choices during reasoning
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return true
	}
	if fr, ok := choice["finish_reason"].(string); ok {
		*lastFinishReason = &fr
	}
	delta, _ := choice["delta"].(map[string]any)
	if reasoning, ok := delta[config.ReasoningField].(string); ok {
		if !emit(StreamEvent{Kind: EventReasoning, Text: reasoning}) {
			return false
		}
	}
	if content, ok := delta["content"]; ok && content != nil {
		switch v := content.(type) {
		case string:
			if !emit(StreamEvent{Kind: EventContent, Text: v}) {
				return false
			}
		default:
			if joined := joinContentParts(content); joined != "" {
				if !emit(StreamEvent{Kind: EventContent, Text: joined}) {
					return false
				}
			}
		}
	}
	if toolCalls, ok := delta["tool_calls"].([]any); ok && len(toolCalls) > 0 {
		mergeToolCalls(toolAcc, toolCalls)
	}
	return true
}

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
