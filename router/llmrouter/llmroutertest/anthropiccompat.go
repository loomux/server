// Package llmroutertest has fakes of the router model's endpoints for
// tests and offline evaluation runs: no network, no keys.
package llmroutertest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// AnthropicCompat fakes Anthropic's OpenAI SDK compatibility endpoint
// (POST https://api.anthropic.com/v1/chat/completions, LOOM-187): the
// Chat Completions protocol, with the differences the router can run
// into. Documented by Anthropic:
//
//   - n must be 1; anything else is a 400.
//   - Some OpenAI fields are accepted and ignored: response_format, a
//     function's strict flag (so a tool call is NOT guaranteed to match
//     the schema), seed, logprobs/top_logprobs, presence/frequency
//     penalties, logit_bias, store, metadata. They're listed in
//     Request.Ignored.
//   - temperature is capped at 1.
//   - There's no refusal field on the message, no cached-token usage
//     detail, and ids are Anthropic's (msg_…, toolu_…).
//
// Assumed, and checked by the trial's smoke step rather than relied on:
//
//   - A request with neither max_tokens nor max_completion_tokens is a
//     400 (the Messages API requires one), so the router sets one.
//   - With RejectForcedToolChoice, a named or "required" tool_choice is
//     a 400, as the native API answers for models that take only auto
//     (Claude Opus 5.5, Sonnet 5.5, Fable 5.1).
//
// Anything but POST /v1/chat/completions is a 404, as a base URL
// without /v1 would get; a missing or wrong bearer key is a 401.
type AnthropicCompat struct {
	// URL is the base URL a tier names: the server's root plus /v1.
	URL string

	key     string
	respond func(Request) Reply
	srv     *httptest.Server

	// RejectForcedToolChoice makes a forced tool_choice a 400.
	RejectForcedToolChoice bool
	// RateLimitFirst answers the first n requests with a 429
	// (retry-after 0).
	RateLimitFirst int32
	// Latency delays every answer.
	Latency time.Duration

	seen     atomic.Int32
	mu       sync.Mutex
	requests []Request
}

// Request is one request the fake received.
type Request struct {
	Path       string
	Authorized bool
	// Body is the decoded JSON body; Bytes its size.
	Body  map[string]any
	Bytes int
	Model string
	// System and User are the last system and user messages' text.
	System, User string
	// ToolName is the first offered function's name; ToolChoice the
	// tool_choice field as sent (nil if absent).
	ToolName   string
	ToolChoice any
	// Ignored lists the fields the endpoint would have ignored.
	Ignored []string
}

// Forced reports whether the request pinned tool_choice to a tool.
func (r Request) Forced() bool {
	switch tc := r.ToolChoice.(type) {
	case string:
		return tc == "required"
	case map[string]any:
		return tc["type"] == "function"
	}
	return false
}

// Reply is what the fake answers a well-formed request with: a call to
// the offered tool with ToolArgs, else Text, else an error Status.
type Reply struct {
	ToolArgs any
	Text     string
	// Status, when set, answers with that HTTP error and ErrMessage.
	Status     int
	ErrMessage string
	// PromptTokens and CompletionTokens override the usage reported;
	// by default it's estimated from the sizes, about 4 bytes a token.
	PromptTokens, CompletionTokens int
}

// ignoredFields are top-level fields the endpoint accepts and ignores.
var ignoredFields = []string{"response_format", "seed", "logprobs", "top_logprobs", "presence_penalty",
	"frequency_penalty", "logit_bias", "store", "metadata"}

// NewAnthropicCompat starts the fake. key is the API key it accepts;
// respond answers each well-formed request. Close it when done.
func NewAnthropicCompat(key string, respond func(Request) Reply) *AnthropicCompat {
	f := &AnthropicCompat{key: key, respond: respond}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	f.URL = f.srv.URL + "/v1"
	return f
}

// Close stops the server.
func (f *AnthropicCompat) Close() { f.srv.Close() }

// Requests returns the requests received so far.
func (f *AnthropicCompat) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func (f *AnthropicCompat) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	req := Request{Path: r.URL.Path, Bytes: len(raw),
		Authorized: r.Header.Get("Authorization") == "Bearer "+f.key}
	_ = json.Unmarshal(raw, &req.Body)
	req.parse()
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	if f.Latency > 0 {
		time.Sleep(f.Latency)
	}

	switch {
	case r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions":
		writeError(w, http.StatusNotFound, "not_found_error", "Not found")
		return
	case !req.Authorized:
		writeError(w, http.StatusUnauthorized, "authentication_error", "invalid x-api-key")
		return
	case f.seen.Add(1) <= f.RateLimitFirst:
		w.Header().Set("Retry-After", "0")
		writeError(w, http.StatusTooManyRequests, "rate_limit_error", "rate limited")
		return
	}
	if n, ok := req.Body["n"].(float64); ok && n != 1 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "n: only n=1 is supported")
		return
	}
	if req.Body["max_tokens"] == nil && req.Body["max_completion_tokens"] == nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "max_tokens: field required")
		return
	}
	if f.RejectForcedToolChoice && req.Forced() {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			`tool_choice: type "tool" and "any" are not supported for this model.`)
		return
	}

	reply := f.respond(req)
	if reply.Status != 0 {
		writeError(w, reply.Status, "api_error", reply.ErrMessage)
		return
	}
	writeCompletion(w, req, reply)
}

func (req *Request) parse() {
	req.Model, _ = req.Body["model"].(string)
	req.ToolChoice = req.Body["tool_choice"]
	msgs, _ := req.Body["messages"].([]any)
	for _, m := range msgs {
		msg, _ := m.(map[string]any)
		text := contentText(msg["content"])
		switch msg["role"] {
		case "system", "developer":
			req.System = text
		case "user":
			req.User = text
		}
	}
	tools, _ := req.Body["tools"].([]any)
	for i, t := range tools {
		fn, _ := t.(map[string]any)["function"].(map[string]any)
		if i == 0 {
			req.ToolName, _ = fn["name"].(string)
		}
		if _, ok := fn["strict"]; ok {
			req.Ignored = append(req.Ignored, "tools[].function.strict")
		}
	}
	for _, k := range ignoredFields {
		if _, ok := req.Body[k]; ok {
			req.Ignored = append(req.Ignored, k)
		}
	}
}

// contentText reads a message's content, a string or text parts.
func contentText(c any) string {
	switch c := c.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, p := range c {
			if part, ok := p.(map[string]any); ok {
				s, _ := part["text"].(string)
				b.WriteString(s)
			}
		}
		return b.String()
	}
	return ""
}

func writeError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": typ, "message": msg}})
}

var replies atomic.Int64

func writeCompletion(w http.ResponseWriter, req Request, reply Reply) {
	n := replies.Add(1)
	msg := map[string]any{"role": "assistant", "content": reply.Text}
	finish, outBytes := "stop", len(reply.Text)
	if reply.ToolArgs != nil {
		args, _ := json.Marshal(reply.ToolArgs)
		msg["content"] = nil
		msg["tool_calls"] = []any{map[string]any{
			"id": fmt.Sprintf("toolu_%024d", n), "type": "function",
			"function": map[string]any{"name": req.ToolName, "arguments": string(args)},
		}}
		finish, outBytes = "tool_calls", len(args)
	}
	prompt, completion := reply.PromptTokens, reply.CompletionTokens
	if prompt == 0 {
		prompt = req.Bytes/4 + 1
	}
	if completion == 0 {
		completion = outBytes/4 + 1
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": fmt.Sprintf("msg_%024d", n), "object": "chat.completion", "created": time.Now().Unix(), "model": req.Model,
		"choices": []any{map[string]any{"index": 0, "finish_reason": finish, "message": msg}},
		"usage":   map[string]any{"prompt_tokens": prompt, "completion_tokens": completion, "total_tokens": prompt + completion},
	})
}

// Client is an HTTP client that sends every request to the fake,
// whatever host it names, so a tier configured exactly as for the live
// endpoint (https://api.anthropic.com/v1) runs against it, and nothing
// leaves the process.
func (f *AnthropicCompat) Client() *http.Client {
	return &http.Client{Transport: redirect{to: f.srv.URL, next: f.srv.Client().Transport}}
}

type redirect struct {
	to   string
	next http.RoundTripper
}

func (r redirect) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := url.Parse(r.to)
	if err != nil {
		return nil, err
	}
	req = req.Clone(req.Context())
	req.URL.Scheme, req.URL.Host, req.Host = u.Scheme, u.Host, u.Host
	return r.next.RoundTrip(req)
}
