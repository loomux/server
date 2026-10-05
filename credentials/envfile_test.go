package credentials

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// LOOM-113: secrets reach the process through a 0600 file written from
// stdin and sourced then deleted, never through a command line.
func TestEnvFile(t *testing.T) {
	home := t.TempDir()
	secrets := map[string]string{
		"API_KEY": "sk-it's\"tricky\" $HOME `x`\nLOOMUX_ENV_line\nEOF",
		"OTHER":   "plain",
	}
	f, err := NewEnvFile("task-1", secrets)
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range secrets {
		if strings.Contains(f.Source, v) || strings.Contains(f.Remove, v) {
			t.Errorf("%s's value is in a command line: %q / %q", name, f.Source, f.Remove)
		}
	}

	sh := func(script string, stdin bool) string {
		t.Helper()
		var cmd *exec.Cmd
		if stdin {
			cmd = exec.Command("sh")
			cmd.Stdin = strings.NewReader(script)
		} else {
			cmd = exec.Command("sh", "-c", script)
		}
		cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("sh %q: %v: %s", script, err, out)
		}
		return string(out)
	}
	sh(f.Write, true)
	path := filepath.Join(home, ".loomux", "env", "task-1")
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("env file: %v, mode %v; want 0600", err, fi.Mode())
	}
	if di, _ := os.Stat(filepath.Dir(path)); di.Mode().Perm() != 0o700 {
		t.Errorf("env dir mode %v, want 0700", di.Mode().Perm())
	}

	got := sh(f.Source+`printf '%s|%s' "$API_KEY" "$OTHER"`, false)
	if got != secrets["API_KEY"]+"|plain" {
		t.Errorf("sourced values = %q", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("env file still there after sourcing: %v", err)
	}
}

func TestEnvFileRejectsBadNames(t *testing.T) {
	if _, err := NewEnvFile("../x", map[string]string{"A": "b"}); err == nil {
		t.Error("accepted a task id that isn't a plain name")
	}
	if _, err := NewEnvFile("t", map[string]string{"A;rm": "b"}); err == nil {
		t.Error("accepted an invalid variable name")
	}
}
