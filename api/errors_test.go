package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Every /api/v1 error body is {error, code}: code is the status's default
// unless the handler names a specific one (api/README.md, "Errors").

func TestErrorCodeFor_DefaultPerStatus(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusBadRequest, "invalid_request"},
		{http.StatusUnauthorized, "unauthorized"},
		{http.StatusForbidden, "forbidden"},
		{http.StatusNotFound, "not_found"},
		{http.StatusConflict, "conflict"},
		{http.StatusRequestEntityTooLarge, "too_large"},
		{http.StatusUnprocessableEntity, "unprocessable"},
		{http.StatusTooManyRequests, "rate_limited"},
		{http.StatusInternalServerError, "internal"},
		{http.StatusNotImplemented, "not_implemented"},
		{http.StatusBadGateway, "bad_gateway"},
		{http.StatusServiceUnavailable, "unavailable"},
		{http.StatusTeapot, "error"},
		{http.StatusGatewayTimeout, "error"},
	} {
		if got := errorCodeFor(tc.status); got != tc.want {
			t.Errorf("errorCodeFor(%d) = %q, want %q", tc.status, got, tc.want)
		}
	}
}

func decodeErrorBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return body
}

func TestWriteError_CarriesErrorAndDefaultCode(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, http.StatusNotFound, "no such thing")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	body := decodeErrorBody(t, rec)
	if body["error"] != "no such thing" || body["code"] != "not_found" || len(body) != 2 {
		t.Fatalf("body = %v, want exactly {error: no such thing, code: not_found}", body)
	}
}

func TestWriteErrorCode_CarriesSpecificCode(t *testing.T) {
	for _, code := range []string{codeConversationBusy, codeIdempotencyConflict, codeUnsupportedAPIVersion} {
		rec := httptest.NewRecorder()
		writeErrorCode(rec, http.StatusConflict, code, "msg")
		body := decodeErrorBody(t, rec)
		if rec.Code != http.StatusConflict || body["error"] != "msg" || body["code"] != code {
			t.Errorf("writeErrorCode(%q) = %d %v", code, rec.Code, body)
		}
	}
}

func TestBusyResponse_KeepsDispatchIDAndAddsCode(t *testing.T) {
	data, err := json.Marshal(busyResponse{
		errorResponse: errorResponse{Error: "busy", Code: codeConversationBusy},
		DispatchID:    "d1",
	})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "busy" || body["code"] != "conversation_busy" || body["dispatch_id"] != "d1" || len(body) != 3 {
		t.Fatalf("busy body = %s, want {error, code: conversation_busy, dispatch_id}", data)
	}
}
