package sdk

import (
	"context"
	"io"
	"strings"
)

// RunForTest runs Main's logic without exiting the process: stdin is
// empty, stdout and stderr discarded, ctx ends a socket server.
func RunForTest(ctx context.Context, p Plugin, args []string) int {
	return RunContext(ctx, p, args, strings.NewReader(""), io.Discard, io.Discard)
}
