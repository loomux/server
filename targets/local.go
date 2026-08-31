package targets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// LocalExecutor runs tmux operations directly on the local machine.
type LocalExecutor struct{}

// NewLocalExecutor constructs a LocalExecutor.
func NewLocalExecutor() *LocalExecutor {
	return &LocalExecutor{}
}

func (e *LocalExecutor) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "tmux", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return "", &exec.Error{Name: "tmux " + args[0], Err: errors.New(firstNonEmpty(stderr.String(), err.Error()))}
	}
	return stdout.String(), nil
}

func (e *LocalExecutor) NewSession(ctx context.Context, session, dir, command string) error {
	args := []string{"new-session", "-d", "-s", session}
	if dir != "" {
		args = append(args, "-c", dir)
	}
	if command != "" {
		args = append(args, command)
	}
	_, err := e.run(ctx, args...)
	return err
}

func (e *LocalExecutor) HasSession(ctx context.Context, session string) (bool, error) {
	cmd := exec.CommandContext(ctx, "tmux", "has-session", "-t", session)
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return false, nil
	}
	return false, err
}

func (e *LocalExecutor) SendKeys(ctx context.Context, target, keys string, enter bool) error {
	if _, err := e.run(ctx, "send-keys", "-t", target, "-l", "--", keys); err != nil {
		return err
	}
	if enter {
		if _, err := e.run(ctx, "send-keys", "-t", target, "Enter"); err != nil {
			return err
		}
	}
	return nil
}

func (e *LocalExecutor) CapturePane(ctx context.Context, target string) (string, error) {
	return e.run(ctx, "capture-pane", "-t", target, "-p")
}

func (e *LocalExecutor) KillSession(ctx context.Context, session string) error {
	_, err := e.run(ctx, "kill-session", "-t", session)
	return err
}

func (e *LocalExecutor) Close() error {
	return nil
}

func (e *LocalExecutor) FileExists(ctx context.Context, path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (e *LocalExecutor) RemoveFile(ctx context.Context, path string) error {
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// RunOnce runs command once via a local shell and returns its combined
// stdout+stderr — e.g. many CLIs print --version to stderr, so both
// streams are captured rather than just stdout.
func (e *LocalExecutor) RunOnce(ctx context.Context, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("targets: run once: %s: %w", command, err)
	}
	return out.String(), nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
