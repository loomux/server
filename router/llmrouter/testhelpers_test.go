package llmrouter

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

type fakeFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type fakeToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function fakeFunctionCall `json:"function"`
}

type fakeMessage struct {
	Role      string         `json:"role"`
	Content   string         `json:"content"`
	ToolCalls []fakeToolCall `json:"tool_calls,omitempty"`
}

type fakeChoice struct {
	Index        int         `json:"index"`
	FinishReason string      `json:"finish_reason"`
	Message      fakeMessage `json:"message"`
}

type fakeChatCompletion struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []fakeChoice `json:"choices"`
}

// newFakeServer starts an httptest.Server backed by handler and returns it
// alongside a request counter, so tests can assert how many times (if any)
// a tier was actually called.
func newFakeServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *int32) {
	t.Helper()
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&count, 1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &count
}

func jsonHandler(t *testing.T, status int, body any) http.HandlerFunc {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(data)
	}
}

// toolCallHandler responds as if the model called functionName with args.
func toolCallHandler(t *testing.T, functionName string, args any) http.HandlerFunc {
	t.Helper()
	argsJSON, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal tool call args: %v", err)
	}
	resp := fakeChatCompletion{
		ID: "chatcmpl-test", Object: "chat.completion", Created: 1, Model: "test-model",
		Choices: []fakeChoice{{
			FinishReason: "tool_calls",
			Message: fakeMessage{
				Role: "assistant",
				ToolCalls: []fakeToolCall{{
					ID:       "call_1",
					Type:     "function",
					Function: fakeFunctionCall{Name: functionName, Arguments: string(argsJSON)},
				}},
			},
		}},
	}
	return jsonHandler(t, http.StatusOK, resp)
}

// malformedToolCallHandler responds with a tool call whose arguments aren't
// valid JSON.
func malformedToolCallHandler(t *testing.T, functionName string) http.HandlerFunc {
	t.Helper()
	resp := fakeChatCompletion{
		ID: "chatcmpl-test", Object: "chat.completion", Created: 1, Model: "test-model",
		Choices: []fakeChoice{{
			FinishReason: "tool_calls",
			Message: fakeMessage{
				Role: "assistant",
				ToolCalls: []fakeToolCall{{
					ID:       "call_1",
					Type:     "function",
					Function: fakeFunctionCall{Name: functionName, Arguments: "{not valid json"},
				}},
			},
		}},
	}
	return jsonHandler(t, http.StatusOK, resp)
}

// noToolCallHandler responds as if the model ignored the forced tool_choice
// and just answered in plain text.
func noToolCallHandler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return textHandler(t, "I won't call a tool.")
}

// textHandler responds with a plain assistant text message, no tool calls.
func textHandler(t *testing.T, text string) http.HandlerFunc {
	t.Helper()
	resp := fakeChatCompletion{
		ID: "chatcmpl-test", Object: "chat.completion", Created: 1, Model: "test-model",
		Choices: []fakeChoice{{
			FinishReason: "stop",
			Message:      fakeMessage{Role: "assistant", Content: text},
		}},
	}
	return jsonHandler(t, http.StatusOK, resp)
}

// errorHandler responds with an HTTP error status the SDK's default retry
// policy will retry (5xx/429) before finally giving up.
func errorHandler(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":{"message":"boom","type":"api_error"}}`))
	}
}

func tierFor(srv *httptest.Server, model string) Tier {
	return Tier{BaseURL: srv.URL, APIKey: "test-key", Model: model}
}
