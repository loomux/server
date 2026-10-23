package kubernetes

import (
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/plugins/rpc"
)

// LOOM-241: a size may request less than its limits (Burstable), so a
// machine fits nodes whose memory is mostly requested already; without
// requests, requests are the limits (Guaranteed), as before.
func TestSizesWithRequests(t *testing.T) {
	cfg, err := parseConfig(baseConfig(t, map[string]any{"sizes": map[string]any{
		"cc":    map[string]any{"cpu": "2", "memory": "4Gi", "disk": "40Gi", "cpu_request": "500m", "memory_request": "1536Mi"},
		"plain": map[string]any{"cpu": "1", "memory": "2Gi", "disk": "10Gi"},
		"same":  map[string]any{"cpu": "1", "memory": "2Gi", "disk": "10Gi", "memory_request": "2Gi"},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	cc, _ := cfg.size("cc")
	if cc.CPURequest != "500m" || cc.MemoryRequest != "1536Mi" {
		t.Errorf("cc = %+v", cc)
	}
	for _, tc := range []struct {
		size                           string
		cpuReq, memReq, cpuLim, memLim string
	}{
		{"cc", "500m", "1536Mi", "2", "4Gi"},
		{"plain", "1", "2Gi", "1", "2Gi"},
		{"same", "1", "2Gi", "1", "2Gi"},
	} {
		size, _ := cfg.size(tc.size)
		sp := spec("abc123")
		sp.Size = tc.size
		res := podFor(sp, size, cfg, testInstance).Spec.Containers[0].Resources
		if res.Requests.Cpu().String() != tc.cpuReq || res.Requests.Memory().String() != tc.memReq ||
			res.Limits.Cpu().String() != tc.cpuLim || res.Limits.Memory().String() != tc.memLim {
			t.Errorf("%s: requests %v, limits %v; want %s/%s, %s/%s", tc.size, res.Requests, res.Limits, tc.cpuReq, tc.memReq, tc.cpuLim, tc.memLim)
		}
	}

	bad := map[string]map[string]any{
		"memory above the limit": {"cpu": "1", "memory": "2Gi", "disk": "10Gi", "memory_request": "3Gi"},
		"cpu above the limit":    {"cpu": "1", "memory": "2Gi", "disk": "10Gi", "cpu_request": "1500m"},
		"not a quantity":         {"cpu": "1", "memory": "2Gi", "disk": "10Gi", "memory_request": "lots"},
		"zero":                   {"cpu": "1", "memory": "2Gi", "disk": "10Gi", "cpu_request": "0"},
	}
	for name, size := range bad {
		if _, err := parseConfig(baseConfig(t, map[string]any{"sizes": map[string]any{"x": size}})); rpcCode(err) != rpc.CodeInvalidConfig {
			t.Errorf("%s: want invalid_config, got %v", name, err)
		}
	}
}

// Describe offers the requests with the sizes, so the host and its UI
// can show what a machine is guaranteed.
func TestDescribeShowsRequests(t *testing.T) {
	cs := fake.NewClientset(append(kindPolicies(t), agentsService())...)
	canaryExits(cs, exitBlocked)
	p := configured(t, cs, map[string]any{"sizes": map[string]any{
		"cc": map[string]any{"cpu": "2", "memory": "4Gi", "disk": "40Gi", "memory_request": "1536Mi"},
	}})
	info, err := p.DescribeTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := protocol.Size{Name: "cc", CPU: "2", Memory: "4Gi", Disk: "40Gi", MemoryRequest: "1536Mi"}
	if len(info.Sizes) != 1 || info.Sizes[0] != want {
		t.Errorf("sizes = %+v, want %+v", info.Sizes, want)
	}
}
