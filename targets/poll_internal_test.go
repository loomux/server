package targets

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/Loomux/server/internal/metrics"
)

func TestParsePoll(t *testing.T) {
	req := PollRequest{
		Files: []string{"/m/a.done", "/m/b.done", "/m/c.done"},
		Panes: []string{"loomux-run", "loomux-dead", "loomux-split", "loomux-gone", "loomux-two words"},
	}
	out := strings.Join([]string{
		"f0",
		"f2",
		"f9", // no such index: ignored
		"p 11 0 loomux-run",
		"p 11 1 loomux-dead",
		// A human split loomux-split's pane: the active one is what
		// PaneExited would read.
		"p 10 0 loomux-split",
		"p 11 1 loomux-split",
		"p 01 0 loomux-split",
		"p 11 0 loomux-two words",
		"p 11 0 loomux-other",
		pollDone,
		"",
	}, "\n")
	res, ok := parsePoll(out, req)
	if !ok {
		t.Fatal("parsePoll: not done, want done")
	}
	if want := []bool{true, false, true}; !slices.Equal(res.Exists, want) {
		t.Errorf("Exists = %v, want %v", res.Exists, want)
	}
	if want := []bool{true, false, false, false, true}; !slices.Equal(res.Running, want) {
		t.Errorf("Running = %v, want %v", res.Running, want)
	}
}

// Output cut short (no pollDone) is no answer: a missing line must not
// be read as "no marker" or "pane gone".
func TestParsePoll_Incomplete(t *testing.T) {
	if _, ok := parsePoll("f0\np 11 0 loomux-run\n", PollRequest{Files: []string{"/m/a"}}); ok {
		t.Error("parsePoll accepted output without its last line")
	}
}

type fakePoller struct {
	LocalExecutor
	polls int
}

func (p *fakePoller) Poll(context.Context, PollRequest) (*PollResult, error) {
	p.polls++
	return &PollResult{}, nil
}

// The metrics wrapper keeps a Poller a Poller, recording its polls as
// op "poll", and doesn't make one of an executor that isn't.
func TestMetricsExecutor_KeepsPoller(t *testing.T) {
	m := metrics.NewDiscard()
	inner := &fakePoller{}
	wrapped, ok := newMetricsExecutor(inner, m, "remote", "jet01").(Poller)
	if !ok {
		t.Fatal("wrapped Poller isn't a Poller")
	}
	if _, err := wrapped.Poll(context.Background(), PollRequest{}); err != nil || inner.polls != 1 {
		t.Fatalf("Poll = %v after %d inner polls, want nil after 1", err, inner.polls)
	}
	if n := opCount(t, m, "poll"); n != "1" {
		t.Errorf("poll ops recorded = %s, want 1", n)
	}
	if _, ok := newMetricsExecutor(&LocalExecutor{}, m, "local", "local").(Poller); ok {
		t.Error("wrapped LocalExecutor is a Poller, want not")
	}
}

// opCount is how many observations of op m's target-op histogram has,
// as scraped.
func opCount(t *testing.T, m *metrics.Metrics, op string) string {
	t.Helper()
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	prefix := `loomux_target_op_seconds_count{kind="remote",op="` + op + `"} `
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	return "0"
}
