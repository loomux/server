package plugintest_test

import (
	"context"
	"io"
	"testing"

	"github.com/Loomux/server/plugins/fake"
	"github.com/Loomux/server/plugins/plugintest"
	"github.com/Loomux/server/plugins/rpc"
	"github.com/Loomux/server/plugins/sdk"
)

// The fake plugin passes its own suite, served in-process over pipes.
func TestFakePassesConformance(t *testing.T) {
	plugintest.Run(t, func(t *testing.T) (*rpc.Conn, func()) {
		hostR, pluginW := io.Pipe()
		pluginR, hostW := io.Pipe()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = sdk.Serve(ctx, fake.New(), pluginR, pluginW)
			_ = pluginW.Close()
		}()
		conn := rpc.NewConn(hostR, hostW, nil)
		return conn, func() {
			conn.Close()
			cancel()
			<-done
		}
	}, plugintest.Options{})
}
