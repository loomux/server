package api_test

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
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

// LOOM-133: bytes trailing a valid JSON body arrive under the deadline
// too, instead of being drained without one after the handler.
func TestRequestBodySlowTrailer(t *testing.T) {
	defer api.SetBodyReadTimeout(300 * time.Millisecond)()
	srv, _, _ := newTestServer(t)
	u, _ := url.Parse(srv.URL)
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	body := `{"password":"wrong"}`
	conn.Write([]byte("POST /api/v1/login HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: " +
		strconv.Itoa(len(body)+50) + "\r\n\r\n" + body))
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	if resp, err := http.ReadResponse(bufio.NewReader(conn), nil); err == nil {
		resp.Body.Close()
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("a stalled trailer held the request for %v", time.Since(start))
	}
}

// LOOM-175: a body over the limit is 413 however malformed it is, whether
// its Content-Length says so up front or it arrives chunked.
func TestRequestBodyTooLarge_Malformed(t *testing.T) {
	srv, _, _ := newTestServer(t)
	junk := strings.Repeat("x", 2<<20)
	cases := map[string]func() (*http.Response, error){
		"content-length": func() (*http.Response, error) {
			return http.Post(srv.URL+"/api/v1/login", "application/json", strings.NewReader(junk))
		},
		"chunked": func() (*http.Response, error) {
			// No length known up front: Go sends it chunked.
			return http.Post(srv.URL+"/api/v1/login", "application/json", io.MultiReader(strings.NewReader(junk)))
		},
		"malformed late": func() (*http.Response, error) {
			return http.Post(srv.URL+"/api/v1/login", "application/json",
				io.MultiReader(strings.NewReader(`{"password":"x"}`+strings.Repeat(" ", 1<<10)+"}"+junk)))
		},
	}
	for name, post := range cases {
		t.Run(name, func(t *testing.T) {
			resp, err := post()
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusRequestEntityTooLarge {
				t.Errorf("oversized malformed login = %d, want 413", resp.StatusCode)
			}
		})
	}
}

// LOOM-175: the body is one JSON value. Whitespace may follow it; a
// second value or anything else is a malformed body, not ignored.
func TestRequestBody_TrailingData(t *testing.T) {
	srv, _, _ := newTestServer(t)
	cases := []struct {
		body string
		want int
	}{
		{`{"password":"` + testPassword + `"}`, http.StatusOK},
		{`{"password":"` + testPassword + `"}` + " \n\t\r\n", http.StatusOK},
		{`{"password":"` + testPassword + `"}{"password":"x"}`, http.StatusBadRequest},
		{`{"password":"` + testPassword + `"} x`, http.StatusBadRequest},
		{`{"password":"` + testPassword + `"}]`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		resp, err := http.Post(srv.URL+"/api/v1/login", "application/json", strings.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("login body %q = %d, want %d", tc.body, resp.StatusCode, tc.want)
		}
	}
}
