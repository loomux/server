package metrics_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Loomux/server/internal/metrics"
)

func TestRecordDispatch_IncrementsCounter(t *testing.T) {
	m := metrics.NewDiscard()
	m.RecordDispatch("answer_directly", metrics.OutcomeSuccess, "")
	m.RecordDispatch("use_workspace", metrics.OutcomeFailure, "target_unreachable")

	body := scrape(t, m)
	if !strings.Contains(body, `loomux_dispatch_total{action="answer_directly",error_class="",outcome="success"} 1`) {
		t.Errorf("expected answer_directly success counter in body:\n%s", body)
	}
	if !strings.Contains(body, `loomux_dispatch_total{action="use_workspace",error_class="target_unreachable",outcome="failure"} 1`) {
		t.Errorf("expected use_workspace failure counter in body:\n%s", body)
	}
}

func TestRecordDispatchDuration_ObservesHistogram(t *testing.T) {
	m := metrics.NewDiscard()
	m.RecordDispatchDuration("total", 50*time.Millisecond)

	body := scrape(t, m)
	if !strings.Contains(body, "loomux_dispatch_stage_seconds_bucket{stage=\"total\"") {
		t.Errorf("expected total stage histogram buckets in body:\n%s", body)
	}
}

func TestRecordTaskTransition_UpdatesGauges(t *testing.T) {
	m := metrics.NewDiscard()
	m.RecordTaskTransition("", "running", "agent")
	m.RecordTaskTransition("running", "awaiting-input", "agent")

	body := scrape(t, m)
	if !strings.Contains(body, `loomux_tasks{status="running"} 0`) {
		t.Errorf("expected running gauge to drop to 0 in body:\n%s", body)
	}
	if !strings.Contains(body, `loomux_tasks{status="awaiting-input"} 1`) {
		t.Errorf("expected awaiting-input gauge to be 1 in body:\n%s", body)
	}
}

func TestSetTargetUp_SetsGauge(t *testing.T) {
	m := metrics.NewDiscard()
	m.SetTargetUp("jet01", "remote", true)
	m.SetTargetUp("local", "local", false)

	body := scrape(t, m)
	if !strings.Contains(body, `loomux_target_up{kind="remote",target="jet01"} 1`) {
		t.Errorf("expected jet01 up gauge in body:\n%s", body)
	}
	if !strings.Contains(body, `loomux_target_up{kind="local",target="local"} 0`) {
		t.Errorf("expected local down gauge in body:\n%s", body)
	}
}

func TestRecordTargetOp_RecordsDurationAndErrors(t *testing.T) {
	m := metrics.NewDiscard()
	m.RecordTargetOp("remote", "capture_pane", 10*time.Millisecond, "", errors.New("boom"))

	body := scrape(t, m)
	if !strings.Contains(body, "loomux_target_op_seconds_bucket{kind=\"remote\",op=\"capture_pane\"") {
		t.Errorf("expected target op histogram in body:\n%s", body)
	}
	if !strings.Contains(body, `loomux_target_op_errors_total{kind="remote",op="capture_pane",reason="other"} 1`) {
		t.Errorf("expected target op error counter in body:\n%s", body)
	}
}

func TestRecordRouterCall_RecordsLatencyAndOutcome(t *testing.T) {
	m := metrics.NewDiscard()
	m.RecordRouterCall(metrics.RouterOpDecide, "primary", metrics.OutcomeSuccess, 5*time.Millisecond)

	body := scrape(t, m)
	if !strings.Contains(body, `loomux_router_calls_total{op="decide",outcome="success",tier="primary"} 1`) {
		t.Errorf("expected router call counter in body:\n%s", body)
	}
	if !strings.Contains(body, "loomux_router_call_seconds_bucket{op=\"decide\"") {
		t.Errorf("expected router call histogram in body:\n%s", body)
	}
}

func TestRecordRouterTokens_RecordsConsumption(t *testing.T) {
	m := metrics.NewDiscard()
	m.RecordRouterTokens("primary", 10, 5, 15)

	body := scrape(t, m)
	if !strings.Contains(body, `loomux_router_tokens_total{kind="prompt",tier="primary"} 10`) {
		t.Errorf("expected prompt token counter in body:\n%s", body)
	}
}

func TestNilMetrics_NoOp(t *testing.T) {
	var m *metrics.Metrics
	m.RecordDispatch("answer_directly", metrics.OutcomeSuccess, "")
	m.RecordTaskTransition("", "running", "agent")
	m.SetTargetUp("t", "remote", true)
	m.RecordRouterCall(metrics.RouterOpRelay, "primary", metrics.OutcomeFailure, time.Millisecond)
	// Reaching here without a panic is the test.
}

func scrape(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}
