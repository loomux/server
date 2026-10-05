package targets

import "testing"

func TestParsePaneDead(t *testing.T) {
	cases := []struct {
		out        string
		wantExited bool
		wantStatus int
		wantSignal int
		wantErr    bool
	}{
		{"0  \n", false, 0, 0, false},
		{"1 127 \n", true, 127, 0, false},
		{"1 0 \n", true, 0, 0, false},
		{"1  9\n", true, -1, 9, false},           // killed by a signal: no exit status
		{"1  \n", true, statusPending, 0, false}, // dead, not reaped yet
		{"1 \n", true, statusPending, 0, false},  // same, from a tmux without pane_dead_signal
		{"", false, 0, 0, true},
		{"1 x \n", false, 0, 0, true},
		{"1  x\n", false, 0, 0, true},
	}
	for _, tc := range cases {
		exited, status, signal, err := parsePaneDead(tc.out)
		if (err != nil) != tc.wantErr || exited != tc.wantExited || status != tc.wantStatus || signal != tc.wantSignal {
			t.Errorf("parsePaneDead(%q) = (%v, %d, %d, %v), want (%v, %d, %d, err=%v)",
				tc.out, exited, status, signal, err, tc.wantExited, tc.wantStatus, tc.wantSignal, tc.wantErr)
		}
	}
}

func TestPaneExitDescribe(t *testing.T) {
	for _, tc := range []struct {
		exit PaneExit
		want string
	}{
		{PaneExit{Status: 3}, "exit 3"},
		{PaneExit{Status: -1, Signal: 9}, "killed by signal 9"},
		{PaneExit{Status: -1}, "an exit status tmux never recorded"},
	} {
		if got := tc.exit.Describe(); got != tc.want {
			t.Errorf("Describe(%+v) = %q, want %q", tc.exit, got, tc.want)
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
