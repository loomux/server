package targets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// LocalExecutor runs tmux operations directly on the local machine.
type LocalExecutor struct{}

// NewLocalExecutor constructs a LocalExecutor.
func NewLocalExecutor() *LocalExecutor {
	return &LocalExecutor{}
}

func (e *LocalExecutor) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "tmux", append([]string{"-L", TmuxSocket}, args...)...)
	cmd.Env = localEnv()
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
	return newSession(ctx, e.run, session, dir, command)
}

func (e *LocalExecutor) PaneExited(ctx context.Context, target string) (*PaneExit, error) {
	return paneExited(ctx, e.run, target)
}

func (e *LocalExecutor) HasSession(ctx context.Context, session string) (bool, error) {
	cmd := exec.CommandContext(ctx, "tmux", "-L", TmuxSocket, "has-session", "-t", session)
	cmd.Env = localEnv()
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

// SendKey sends one key by its tmux name to target.
func (e *LocalExecutor) SendKey(ctx context.Context, target, key string) error {
	if _, err := e.run(ctx, "send-keys", "-t", target, key); err != nil {
		return fmt.Errorf("targets: send key: %w", err)
	}
	return nil
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

func (e *LocalExecutor) PasteText(ctx context.Context, target, text string, enter bool) error {
	text, err := checkPaste(text)
	if err != nil {
		return err
	}
	buf := pasteBuffer(target)
	load := exec.CommandContext(ctx, "tmux", "-L", TmuxSocket, "load-buffer", "-b", buf, "-")
	load.Env = localEnv()
	load.Stdin = strings.NewReader(text)
	var stderr bytes.Buffer
	load.Stderr = &stderr
	if err := load.Run(); err != nil {
		return &exec.Error{Name: "tmux load-buffer", Err: errors.New(firstNonEmpty(stderr.String(), err.Error()))}
	}
	if _, err := e.run(ctx, "paste-buffer", "-p", "-d", "-b", buf, "-t", target); err != nil {
		// -d deletes only on a paste that worked: don't leave the message
		// readable on the tmux server (show-buffer).
		_, _ = e.run(context.WithoutCancel(ctx), "delete-buffer", "-b", buf)
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
	return e.run(ctx, captureArgs(target)...)
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
	// The script goes to sh on stdin, as it does over SSH, not as an
	// argument: what it carries (an env file's secrets, LOOM-113) stays
	// off the process list.
	cmd := exec.CommandContext(ctx, "sh")
	cmd.Env = localEnv()
	cmd.Stdin = strings.NewReader("{\n" + command + "\n} </dev/null\n")
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
