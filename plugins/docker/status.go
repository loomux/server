package docker

import (
	"context"
	"encoding/binary"
	"io"
	"net/url"
	"strconv"
	"strings"
)

// statusOf maps a container to an environment status and the plugin's
// words for it (design §9: health = inspect state, exit code, restart
// count). A healthy container is one whose health check (sshd
// answering) passes: the readiness probe's analogue.
func statusOf(c *containerInspect) (status, reason string) {
	switch c.State.Status {
	case "running":
		health := ""
		if c.State.Health != nil {
			health = c.State.Health.Status
		}
		switch health {
		case "healthy":
			return "running", ""
		case "unhealthy":
			return "error", "sshd isn't answering (the container is unhealthy)"
		default:
			return "starting", "sshd isn't answering yet"
		}
	case "created":
		return "creating", "not started yet"
	case "restarting":
		if c.RestartCount >= 2 {
			return "error", "sshd keeps exiting (exit " + strconv.Itoa(c.State.ExitCode) + ")"
		}
		return "starting", "restarting (exit " + strconv.Itoa(c.State.ExitCode) + ")"
	case "exited":
		if c.State.ExitCode != 0 {
			return "stopped", "exit " + strconv.Itoa(c.State.ExitCode)
		}
		return "stopped", ""
	case "paused":
		return "stopped", "paused"
	case "removing":
		return "destroying", ""
	case "dead":
		return "error", "the container is dead" + errorSuffix(c.State.Error)
	}
	return "creating", c.State.Status
}

func errorSuffix(s string) string {
	if s = oneLine(s); s != "" {
		return ": " + s
	}
	return ""
}

// portTaken reports a start refused because the published port is
// held by something else.
func portTaken(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "port is already allocated") || strings.Contains(s, "address already in use")
}

// digest is the registry digest of the image a container runs (its
// RepoDigests), cached by image id; "" when the engine knows none (an
// image built locally).
func (p *Plugin) digest(ctx context.Context, eng *engine, imageID string) string {
	if imageID == "" {
		return ""
	}
	p.mu.Lock()
	d, ok := p.digests[imageID]
	p.mu.Unlock()
	if ok {
		return d
	}
	var img imageInspect
	if err := eng.get(ctx, "/images/"+url.PathEscape(imageID)+"/json", nil, &img); err != nil {
		return ""
	}
	for _, rd := range img.RepoDigests {
		if i := strings.Index(rd, "@sha256:"); i >= 0 {
			d = rd[i+1:]
			break
		}
	}
	p.mu.Lock()
	p.digests[imageID] = d
	p.mu.Unlock()
	return d
}

// logTail is the last line a container wrote (its entrypoint's
// complaint when it keeps exiting), one line, bounded.
func (p *Plugin) logTail(ctx context.Context, eng *engine, name string) string {
	body, err := eng.stream(ctx, "GET", "/containers/"+name+"/logs", url.Values{"stdout": {"1"}, "stderr": {"1"}, "tail": {"5"}})
	if err != nil {
		return ""
	}
	defer body.Close()
	raw, _ := io.ReadAll(io.LimitReader(body, 16<<10))
	text := demux(raw)
	lines := strings.Split(strings.TrimSpace(text), "\n")
	return oneLine(lines[len(lines)-1])
}

// demux strips the stream's 8-byte frame headers (a non-tty
// container's log is multiplexed), falling back to the raw text.
func demux(raw []byte) string {
	var out strings.Builder
	rest := raw
	for len(rest) >= 8 {
		if rest[0] > 2 || rest[1] != 0 || rest[2] != 0 || rest[3] != 0 {
			return string(raw)
		}
		n := int(binary.BigEndian.Uint32(rest[4:8]))
		rest = rest[8:]
		if n > len(rest) {
			n = len(rest)
		}
		out.Write(rest[:n])
		rest = rest[n:]
	}
	if out.Len() == 0 {
		return string(raw)
	}
	return out.String()
}
