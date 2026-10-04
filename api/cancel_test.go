package api_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
)

// LOOM-99: POST /dispatches/{id}/cancel stops a running turn, which then
// ends failed with class cancelled.
func TestCancelDispatch(t *testing.T) {
	srv, dispatcher, _ := newTestServer(t)
	started := make(chan struct{})
	dispatcher.DispatchFunc = func(ctx context.Context, c, m, h string) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}
	token, _ := login(t, srv.URL, testPassword)
	_, d := mustPostDispatch(t, srv.URL+"/api/v1/dispatch", token, map[string]string{"conversation_id": "c1", "message": "go"},
		map[string]string{"Prefer": "respond-async"})
	<-started

	resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/dispatches/"+d.DispatchID+"/cancel", token, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("cancel status = %d, want 202", resp.StatusCode)
	}
	done := pollDispatch(t, srv.URL, token, d.DispatchID)
	if done.Status != "failed" || done.ErrorClass != "cancelled" {
		t.Fatalf("dispatch = %+v, want failed/cancelled", done)
	}

	// Ended: a second cancel conflicts; an unknown one is 404.
	resp = authedRequest(t, http.MethodPost, srv.URL+"/api/v1/dispatches/"+d.DispatchID+"/cancel", token, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("cancel of an ended dispatch = %d, want 409", resp.StatusCode)
	}
	resp = authedRequest(t, http.MethodPost, srv.URL+"/api/v1/dispatches/nope/cancel", token, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("cancel of an unknown dispatch = %d, want 404", resp.StatusCode)
	}
}

type fakeTaskCanceller struct {
	err error
	ids []string
}

func (f *fakeTaskCanceller) CancelTask(ctx context.Context, id string) (string, error) {
	f.ids = append(f.ids, id)
	return "d-1", f.err
}

func TestCancelTask(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"running", nil, http.StatusAccepted},
		{"unknown", registry.ErrNotFound, http.StatusNotFound},
		{"ended", orchestrator.ErrTaskInactive, http.StatusConflict},
		{"taken over", orchestrator.ErrHumanTakeover, http.StatusConflict},
		{"broken", errors.New("boom"), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &fakeTaskCanceller{err: tc.err}
			srv, _, _ := newTestServer(t, api.WithTaskCanceller(c))
			token, _ := login(t, srv.URL, testPassword)
			resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/tasks/t1/cancel", token, nil)
			resp.Body.Close()
			if resp.StatusCode != tc.want || len(c.ids) != 1 || c.ids[0] != "t1" {
				t.Errorf("status = %d (calls %v), want %d", resp.StatusCode, c.ids, tc.want)
			}
		})
	}
	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)
	resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/tasks/t1/cancel", token, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("without a canceller: %d, want 501", resp.StatusCode)
	}
	resp = authedRequest(t, http.MethodPost, srv.URL+"/api/v1/tasks/t1/cancel", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated: %d, want 401", resp.StatusCode)
	}
}
