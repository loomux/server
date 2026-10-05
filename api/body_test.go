package api_test

import (
	"bufio"
	"bytes"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Loomux/server/api"
)

// LOOM-132: a request body is bounded in size and in how long it may
// take to arrive, the unauthenticated login included.
func TestRequestBodyTooLarge(t *testing.T) {
	srv, _, _ := newTestServer(t)
	big := `{"password":"` + strings.Repeat("a", 2<<20) + `"}`
	resp, err := http.Post(srv.URL+"/api/v1/login", "application/json", bytes.NewReader([]byte(big)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized login = %d, want 413", resp.StatusCode)
	}

	token, _ := login(t, srv.URL, testPassword)
	r := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/dispatch", token,
		[]byte(`{"conversation_id":"c","message":"`+strings.Repeat("m", 2<<20)+`"}`))
	r.Body.Close()
	if r.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized dispatch = %d, want 413", r.StatusCode)
	}
}

func TestRequestBodySlow(t *testing.T) {
	defer api.SetBodyReadTimeout(300 * time.Millisecond)()
	srv, _, _ := newTestServer(t)
	u, _ := url.Parse(srv.URL)
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Headers promise a body that never comes.
	conn.Write([]byte("POST /api/v1/login HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"pass"))
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Errorf("slow body answered %d", resp.StatusCode)
		}
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("slow body held the request for %v", time.Since(start))
	}
}
