package targets

import "testing"

func TestParsePaneDead(t *testing.T) {
	cases := []struct {
		out        string
		wantExited bool
		wantStatus int
		wantErr    bool
	}{
		{"0  \n", false, 0, false},
		{"1 127 \n", true, 127, false},
		{"1 0 \n", true, 0, false},
		{"1  9\n", true, -1, false},           // killed by a signal: no exit status
		{"1  \n", true, statusPending, false}, // dead, not reaped yet
		{"1 \n", true, statusPending, false},  // same, from a tmux without pane_dead_signal
		{"", false, 0, true},
		{"1 x\n", false, 0, true},
	}
	for _, tc := range cases {
		exited, status, err := parsePaneDead(tc.out)
		if (err != nil) != tc.wantErr || exited != tc.wantExited || status != tc.wantStatus {
			t.Errorf("parsePaneDead(%q) = (%v, %d, %v), want (%v, %d, err=%v)",
				tc.out, exited, status, err, tc.wantExited, tc.wantStatus, tc.wantErr)
		}
	}
}

func TestTrimDeadPaneOutput(t *testing.T) {
	in := "hi\nthere\n\n\n\nPane is dead (status 0, Fri Oct  2 22:50:07 2026)\n"
	if got := trimDeadPaneOutput(in); got != "hi\nthere" {
		t.Errorf("trimDeadPaneOutput = %q, want %q", got, "hi\nthere")
	}
	if got := trimDeadPaneOutput("Pane is dead (status 127, x)\n"); got != "" {
		t.Errorf("trimDeadPaneOutput(only the dead line) = %q, want empty", got)
	}
}
