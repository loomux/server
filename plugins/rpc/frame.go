// Package rpc is the wire layer of loomux-plugin/1
// (docs/design/target-providers.md §1.3): JSON-RPC 2.0 messages in
// Content-Length frames, the LSP convention, over a subprocess's stdio
// or a unix socket. Conn is the host's side, Serve a plugin's.
package rpc

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// MaxFrameBytes is the largest payload either side accepts.
const MaxFrameBytes = 1 << 20

// maxHeaderLine bounds one header line: a frame's headers are tiny.
const maxHeaderLine = 256

var (
	// ErrFrameTooLarge is a frame whose Content-Length is over
	// MaxFrameBytes.
	ErrFrameTooLarge = errors.New("rpc: frame too large")
	// ErrBadFrame is bytes that aren't a frame: a stray print to stdout
	// before the first frame, say.
	ErrBadFrame = errors.New("rpc: not a frame")
)

// WriteFrame writes payload as one frame.
func WriteFrame(w io.Writer, payload []byte) error {
	if len(payload) > MaxFrameBytes {
		return ErrFrameTooLarge
	}
	_, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(payload), payload)
	return err
}

// ReadFrame reads one frame's payload. A header line that isn't
// Content-Length or Content-Type, a missing length, or anything before
// the headers is ErrBadFrame; a length over MaxFrameBytes is
// ErrFrameTooLarge (nothing of it is read, so the stream is unusable
// afterwards, which is the point).
func ReadFrame(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := readHeaderLine(r)
		if err != nil {
			return nil, err
		}
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("%w: header %q", ErrBadFrame, truncate(line))
		}
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "content-length":
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil || n < 0 {
				return nil, fmt.Errorf("%w: Content-Length %q", ErrBadFrame, strings.TrimSpace(value))
			}
			length = n
		case "content-type":
		default:
			return nil, fmt.Errorf("%w: header %q", ErrBadFrame, truncate(name))
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("%w: no Content-Length", ErrBadFrame)
	}
	if length > MaxFrameBytes {
		return nil, fmt.Errorf("%w: %d bytes, the limit is %d", ErrFrameTooLarge, length, MaxFrameBytes)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

// readHeaderLine reads one CRLF- (or LF-) terminated line of at most
// maxHeaderLine bytes, without the terminator.
func readHeaderLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) || len(line) > maxHeaderLine {
		return "", fmt.Errorf("%w: header line too long", ErrBadFrame)
	}
	if err != nil {
		if err == io.EOF && len(line) > 0 {
			return "", fmt.Errorf("%w: unterminated header", ErrBadFrame)
		}
		return "", err
	}
	return strings.TrimRight(string(line), "\r\n"), nil
}

func truncate(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}
