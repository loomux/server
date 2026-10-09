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
	"strings"
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

// TargetProvider is what a plugin implements to make machines on demand
// (the targets.* group, docs/design/target-providers.md §1.3). Its
// manifest must declare targets.create (and the other targets.*
// capabilities it supports); the host never calls what isn't declared.
// Every method is idempotent on its id and safe to repeat after a crash.
type TargetProvider interface {
	DescribeTargets(ctx context.Context) (protocol.TargetsInfo, error)
	CreateTarget(ctx context.Context, spec protocol.EnvironmentSpec) (protocol.Environment, error)
	GetTarget(ctx context.Context, id string) (protocol.Environment, error)
	ListTargets(ctx context.Context) ([]protocol.Environment, error)
	StartTarget(ctx context.Context, id string) error
	StopTarget(ctx context.Context, id string) error
	RecreateTarget(ctx context.Context, id string, spec protocol.EnvironmentSpec) (protocol.Environment, error)
	DestroyTarget(ctx context.Context, id string) error
	TargetHealth(ctx context.Context, id string) (protocol.EnvironmentHealth, error)
	TargetAttachCommands(ctx context.Context, id, session string) ([]protocol.AttachCommand, error)
}

// Handler returns the rpc.Handler that serves p's plugin.* methods, and
// targets.* when p implements TargetProvider. Methods of other groups
// answer method_not_found unless extra serves them (nil for none).
func Handler(p Plugin, extra rpc.Handler) rpc.Handler {
	targets, _ := p.(TargetProvider)
	return func(ctx context.Context, method string, params json.RawMessage) (any, *rpc.Error) {
		if targets != nil && strings.HasPrefix(method, "targets.") {
			return targetsHandler(ctx, targets, method, params)
		}
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
	return RunContext(ctx, p, args, stdin, stdout, stderr)
}

// RunContext is Main without the process: it serves p on stdin/stdout,
// or on the socket args name with --listen, until the exchange ends or
// ctx is done, and returns the exit code Main would exit with. Tests of
// the host use it to serve a plugin in-process.
func RunContext(ctx context.Context, p Plugin, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
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

// targetsHandler serves the targets.* group.
func targetsHandler(ctx context.Context, t TargetProvider, method string, params json.RawMessage) (any, *rpc.Error) {
	decode := func(v any) *rpc.Error {
		if len(params) == 0 {
			return &rpc.Error{Code: rpc.CodeInvalidParams, Message: method + ": parameters are required"}
		}
		if err := json.Unmarshal(params, v); err != nil {
			return &rpc.Error{Code: rpc.CodeInvalidParams, Message: method + ": " + err.Error()}
		}
		return nil
	}
	switch method {
	case protocol.MethodTargetsDescribe:
		info, err := t.DescribeTargets(ctx)
		if err != nil {
			return nil, toRPC(err)
		}
		if info.Sizes == nil {
			info.Sizes = []protocol.Size{}
		}
		if info.EgressOptions == nil {
			info.EgressOptions = []string{}
		}
		return info, nil
	case protocol.MethodTargetsCreate:
		var spec protocol.EnvironmentSpec
		if e := decode(&spec); e != nil {
			return nil, e
		}
		if spec.ID == "" {
			return nil, &rpc.Error{Code: rpc.CodeInvalidParams, Message: "targets.create: id is required"}
		}
		env, err := t.CreateTarget(ctx, spec)
		if err != nil {
			return nil, toRPC(err)
		}
		return env, nil
	case protocol.MethodTargetsGet, protocol.MethodTargetsStart, protocol.MethodTargetsStop, protocol.MethodTargetsDestroy, protocol.MethodTargetsHealth:
		var p protocol.IDParams
		if e := decode(&p); e != nil {
			return nil, e
		}
		if p.ID == "" {
			return nil, &rpc.Error{Code: rpc.CodeInvalidParams, Message: method + ": id is required"}
		}
		switch method {
		case protocol.MethodTargetsGet:
			env, err := t.GetTarget(ctx, p.ID)
			if err != nil {
				return nil, toRPC(err)
			}
			return env, nil
		case protocol.MethodTargetsStart:
			if err := t.StartTarget(ctx, p.ID); err != nil {
				return nil, toRPC(err)
			}
		case protocol.MethodTargetsStop:
			if err := t.StopTarget(ctx, p.ID); err != nil {
				return nil, toRPC(err)
			}
		case protocol.MethodTargetsDestroy:
			if err := t.DestroyTarget(ctx, p.ID); err != nil {
				return nil, toRPC(err)
			}
		case protocol.MethodTargetsHealth:
			h, err := t.TargetHealth(ctx, p.ID)
			if err != nil {
				return nil, toRPC(err)
			}
			return h, nil
		}
		return struct{}{}, nil
	case protocol.MethodTargetsList:
		envs, err := t.ListTargets(ctx)
		if err != nil {
			return nil, toRPC(err)
		}
		if envs == nil {
			envs = []protocol.Environment{}
		}
		return protocol.ListResult{Environments: envs}, nil
	case protocol.MethodTargetsRecreate:
		var p protocol.RecreateParams
		if e := decode(&p); e != nil {
			return nil, e
		}
		if p.ID == "" {
			return nil, &rpc.Error{Code: rpc.CodeInvalidParams, Message: "targets.recreate: id is required"}
		}
		env, err := t.RecreateTarget(ctx, p.ID, p.Spec)
		if err != nil {
			return nil, toRPC(err)
		}
		return env, nil
	case protocol.MethodTargetsAttachCommands:
		var p protocol.AttachParams
		if e := decode(&p); e != nil {
			return nil, e
		}
		cmds, err := t.TargetAttachCommands(ctx, p.ID, p.Session)
		if err != nil {
			return nil, toRPC(err)
		}
		if cmds == nil {
			cmds = []protocol.AttachCommand{}
		}
		return protocol.AttachResult{Commands: cmds}, nil
	}
	return nil, &rpc.Error{Code: rpc.CodeMethodNotFound, Message: "no method " + method}
}
