// Package sdk is what a plugin's main calls to speak loomux-plugin/1
// (docs/design/target-providers.md §1.3): implement Plugin, hand it to
// Main, and the plugin serves stdio when started with no arguments or a
// unix socket with --listen <path> (the sidecar form). The host, not the
// plugin, drives every exchange.
package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/Loomux/server/plugins"
	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/plugins/rpc"
)

// Plugin is what a plugin implements. An error that is an *rpc.Error
// reaches the host with its code; any other error is reported as
// "internal" with its text, so keep that text free of secrets.
type Plugin interface {
	// Describe returns the manifest: the handshake. It must equal the
	// plugin.json shipped beside the executable.
	Describe(ctx context.Context) (*plugins.Manifest, error)
	// Configure receives the configuration (secrets included) and the
	// host's details. It is called at every start and after a change.
	Configure(ctx context.Context, p protocol.ConfigureParams) error
	// Check verifies credentials and reachability.
	Check(ctx context.Context) (protocol.CheckResult, error)
	// Shutdown asks the plugin to stop cleanly; the host closes the
	// channel afterwards.
	Shutdown(ctx context.Context) error
}

// Handler returns the rpc.Handler that serves p's plugin.* methods.
// Methods of other groups answer method_not_found unless extra serves
// them (nil for none): a later capability adds its own group that way.
func Handler(p Plugin, extra rpc.Handler) rpc.Handler {
	return func(ctx context.Context, method string, params json.RawMessage) (any, *rpc.Error) {
		switch method {
		case protocol.MethodDescribe:
			m, err := p.Describe(ctx)
			if err != nil {
				return nil, toRPC(err)
			}
			return m, nil
		case protocol.MethodConfigure:
			var cp protocol.ConfigureParams
			if err := json.Unmarshal(params, &cp); err != nil {
				return nil, &rpc.Error{Code: rpc.CodeInvalidParams, Message: "configure: " + err.Error()}
			}
			if err := p.Configure(ctx, cp); err != nil {
				return nil, toRPC(err)
			}
			return struct{}{}, nil
		case protocol.MethodCheck:
			res, err := p.Check(ctx)
			if err != nil {
				return nil, toRPC(err)
			}
			if res.Problems == nil {
				res.Problems = []protocol.Problem{}
			}
			return res, nil
		case protocol.MethodShutdown:
			if err := p.Shutdown(ctx); err != nil {
				return nil, toRPC(err)
			}
			return struct{}{}, nil
		}
		if extra != nil {
			return extra(ctx, method, params)
		}
		return nil, &rpc.Error{Code: rpc.CodeMethodNotFound, Message: "no method " + method}
	}
}

func toRPC(err error) *rpc.Error {
	var e *rpc.Error
	if errors.As(err, &e) {
		return e
	}
	return &rpc.Error{Code: rpc.CodeInternal, Message: err.Error()}
}

// Serve serves p on one connection (r in, w out) until it ends.
func Serve(ctx context.Context, p Plugin, r io.Reader, w io.Writer) error {
	return rpc.Serve(ctx, r, w, Handler(p, nil))
}

// Main runs p as a plugin process: on stdio, or on the unix socket
// --listen names, until the host closes the channel or SIGTERM/SIGINT
// arrives. It exits the process: 0 when the host ended the exchange,
// 1 on a transport error, 2 on a bad flag.
func Main(p Plugin) {
	os.Exit(run(p, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(p Plugin, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return runWithContext(ctx, p, args, stdin, stdout, stderr)
}

func runWithContext(ctx context.Context, p Plugin, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("plugin", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "", "serve on this unix socket instead of stdio (the sidecar form)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *listen == "" {
		if err := Serve(ctx, p, stdin, stdout); err != nil && ctx.Err() == nil {
			fmt.Fprintln(stderr, "plugin: stdio:", err)
			return 1
		}
		return 0
	}
	if err := serveSocket(ctx, p, *listen, stderr); err != nil {
		fmt.Fprintln(stderr, "plugin: socket:", err)
		return 1
	}
	return 0
}

// serveSocket listens on path (replacing a stale socket file), mode
// 0660 so the host's group can connect, and serves every connection
// until ctx is done.
func serveSocket(ctx context.Context, p Plugin, path string, stderr io.Writer) error {
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		l.Close()
		return err
	}
	go func() {
		<-ctx.Done()
		l.Close()
	}()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer conn.Close()
			if err := Serve(ctx, p, conn, conn); err != nil && ctx.Err() == nil {
				fmt.Fprintln(stderr, "plugin: connection:", err)
			}
		}()
	}
}
