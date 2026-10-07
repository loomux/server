package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Loomux/server/registry"
)

// Lists and paging (API v1 freeze review item 17): the conversation list
// and a conversation's messages carry has_more, always false without
// ?limit=, and page by the task transcript's convention when asked.

type pagedConversation struct {
	Messages []struct {
		ID string `json:"id"`
	} `json:"messages"`
	Tasks      []any   `json:"tasks"`
	HasMore    *bool   `json:"has_more"`
	NextBefore *string `json:"next_before"`
}

func getPagedConversation(t *testing.T, url, token string) (int, pagedConversation) {
	t.Helper()
	resp := authedRequest(t, http.MethodGet, url, token, nil)
	defer resp.Body.Close()
	var out pagedConversation
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return resp.StatusCode, out
}

func messageIDs(p pagedConversation) string {
	ids := make([]string, 0, len(p.Messages))
	for _, m := range p.Messages {
		ids = append(ids, m.ID)
	}
	return fmt.Sprint(ids)
}

func TestGetConversation_HasMoreFalseWithoutLimit(t *testing.T) {
	srv, _, store := newTestServer(t)
	createTestMessage(t, store, "m1", "conv", "", registry.MessageRoleUser, "hi")
	createTestMessage(t, store, "m2", "conv", "", registry.MessageRoleAssistant, "hello")

	token, _ := login(t, srv.URL, testPassword)
	status, out := getPagedConversation(t, srv.URL+"/api/v1/conversations/conv", token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if out.HasMore == nil || *out.HasMore {
		t.Fatalf("has_more = %v, want present and false", out.HasMore)
	}
	if out.NextBefore != nil {
		t.Fatalf("next_before = %q, want it omitted", *out.NextBefore)
	}
	if got := messageIDs(out); got != "[m1 m2]" {
		t.Fatalf("messages = %v, want [m1 m2]", got)
	}
}

func TestGetConversation_PagesMessages(t *testing.T) {
	srv, _, store := newTestServer(t)
	ws := createTestWorkspace(t, store, "ws1", registry.WorkspaceStatusIdle)
	createTestTask(t, store, "task-1", ws.ID, "conv", registry.TaskStatusCompleted)
	for i := 1; i <= 5; i++ {
		createTestMessage(t, store, fmt.Sprintf("m%d", i), "conv", "", registry.MessageRoleUser, fmt.Sprintf("msg %d", i))
		time.Sleep(2 * time.Millisecond)
	}
	token, _ := login(t, srv.URL, testPassword)
	base := srv.URL + "/api/v1/conversations/conv"

	// The latest two, oldest first; more remain before m4.
	status, out := getPagedConversation(t, base+"?limit=2", token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if got := messageIDs(out); got != "[m4 m5]" {
		t.Fatalf("limit=2 messages = %v, want [m4 m5]", got)
	}
	if out.HasMore == nil || !*out.HasMore || out.NextBefore == nil || *out.NextBefore != "m4" {
		t.Fatalf("limit=2 has_more=%v next_before=%v, want true, m4", out.HasMore, out.NextBefore)
	}
	if len(out.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1 (only messages page)", len(out.Tasks))
	}

	// Following next_before.
	_, out = getPagedConversation(t, base+"?limit=2&before=m4", token)
	if got := messageIDs(out); got != "[m2 m3]" || !*out.HasMore || out.NextBefore == nil || *out.NextBefore != "m2" {
		t.Fatalf("before=m4 page = %v has_more=%v next_before=%v, want [m2 m3], true, m2", got, *out.HasMore, out.NextBefore)
	}

	// The last page: has_more false, no next_before.
	_, out = getPagedConversation(t, base+"?limit=2&before=m2", token)
	if got := messageIDs(out); got != "[m1]" || *out.HasMore || out.NextBefore != nil {
		t.Fatalf("before=m2 page = %v has_more=%v next_before=%v, want [m1], false, omitted", got, *out.HasMore, out.NextBefore)
	}

	// before alone: everything before it, nothing more.
	_, out = getPagedConversation(t, base+"?before=m3", token)
	if got := messageIDs(out); got != "[m1 m2]" || *out.HasMore {
		t.Fatalf("before=m3 page = %v has_more=%v, want [m1 m2], false", got, *out.HasMore)
	}

	// A limit covering everything: has_more false.
	_, out = getPagedConversation(t, base+"?limit=5", token)
	if got := messageIDs(out); got != "[m1 m2 m3 m4 m5]" || *out.HasMore {
		t.Fatalf("limit=5 page = %v has_more=%v, want all five, false", got, *out.HasMore)
	}
}

func TestGetConversation_PagingErrors(t *testing.T) {
	srv, _, store := newTestServer(t)
	createTestMessage(t, store, "m1", "conv", "", registry.MessageRoleUser, "hi")
	createTestMessage(t, store, "m-other", "conv-other", "", registry.MessageRoleUser, "elsewhere")
	token, _ := login(t, srv.URL, testPassword)

	// As the task transcript: a bad limit, or a before naming no message
	// of this conversation, is a 400.
	for _, q := range []string{"limit=0", "limit=-1", "limit=x", "limit=501",
		"before=nope", "before=m-other"} {
		status, _ := getPagedConversation(t, srv.URL+"/api/v1/conversations/conv?"+q, token)
		if status != http.StatusBadRequest {
			t.Errorf("?%s: status = %d, want 400", q, status)
		}
	}
	// An unknown conversation is still a 404, paging or not.
	if status, _ := getPagedConversation(t, srv.URL+"/api/v1/conversations/nope?before=m1", token); status != http.StatusNotFound {
		t.Errorf("unknown conversation with before: status = %d, want 404", status)
	}
}

type pagedList struct {
	Conversations []conversationSummary `json:"conversations"`
	HasMore       *bool                 `json:"has_more"`
}

func getPagedList(t *testing.T, url, token string) (int, pagedList) {
	t.Helper()
	resp := authedRequest(t, http.MethodGet, url, token, nil)
	defer resp.Body.Close()
	var out pagedList
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return resp.StatusCode, out
}

func TestListConversations_HasMoreAndLimit(t *testing.T) {
	srv, _, store := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)

	// Empty: has_more present and false.
	status, out := getPagedList(t, srv.URL+"/api/v1/conversations", token)
	if status != http.StatusOK || out.HasMore == nil || *out.HasMore {
		t.Fatalf("empty list: status=%d has_more=%v, want 200, false", status, out.HasMore)
	}

	for i := 1; i <= 3; i++ {
		createTestMessage(t, store, fmt.Sprintf("m%d", i), fmt.Sprintf("conv-%d", i), "", registry.MessageRoleUser, fmt.Sprintf("first of %d", i))
		time.Sleep(5 * time.Millisecond)
	}

	_, out = getPagedList(t, srv.URL+"/api/v1/conversations", token)
	if len(out.Conversations) != 3 || out.HasMore == nil || *out.HasMore {
		t.Fatalf("no limit: %d conversations has_more=%v, want 3, false", len(out.Conversations), out.HasMore)
	}

	// limit keeps the most recently updated first, previews included.
	_, out = getPagedList(t, srv.URL+"/api/v1/conversations?limit=2", token)
	if len(out.Conversations) != 2 || !*out.HasMore {
		t.Fatalf("limit=2: %d conversations has_more=%v, want 2, true", len(out.Conversations), *out.HasMore)
	}
	if out.Conversations[0].ConversationID != "conv-3" || out.Conversations[1].ConversationID != "conv-2" {
		t.Fatalf("limit=2 order = [%s %s], want [conv-3 conv-2]", out.Conversations[0].ConversationID, out.Conversations[1].ConversationID)
	}
	if out.Conversations[0].Preview != "first of 3" {
		t.Fatalf("limit=2 preview = %q, want %q", out.Conversations[0].Preview, "first of 3")
	}

	_, out = getPagedList(t, srv.URL+"/api/v1/conversations?limit=3", token)
	if len(out.Conversations) != 3 || *out.HasMore {
		t.Fatalf("limit=3: %d conversations has_more=%v, want 3, false", len(out.Conversations), *out.HasMore)
	}

	for _, q := range []string{"limit=0", "limit=x", "limit=501"} {
		if status, _ := getPagedList(t, srv.URL+"/api/v1/conversations?"+q, token); status != http.StatusBadRequest {
			t.Errorf("?%s: status = %d, want 400", q, status)
		}
	}
}
