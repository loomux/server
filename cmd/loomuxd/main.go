// Command loomuxd is the Loomux server process: a thin CLI wrapper
// around app.Build's fully wired domain layer. The versioned HTTP/WS
// client API and login-gated auth (design spec §9, §10 axis 1, LOOM-9)
// aren't built yet, so this binary's "minimal internal interface" is a
// local one — a single message via -message, or a line-by-line stdin
// loop otherwise — proving the whole stack constructs and is
// dispatchable without exposing anything over the network.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/google/uuid"

	"github.com/Loomux/server/app"
)

func main() {
	message := flag.String("message", "", "dispatch a single message and exit, instead of reading chat messages from stdin")
	conversation := flag.String("conversation", "", "conversation ID to use (default: a freshly generated one)")
	flag.Parse()

	cfg, err := app.LoadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if cfg.MasterKey == nil {
		fmt.Fprintln(os.Stderr, "loomuxd: warning: LOOMUX_MASTER_KEY is not set — credential vault operations will fail until it is")
	}

	loomux, err := app.Build(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer loomux.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	convID := *conversation
	if convID == "" {
		convID = uuid.NewString()
	}

	if *message != "" {
		reply, err := loomux.Dispatch(ctx, convID, *message)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(reply)
		return
	}

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		reply, err := loomux.Dispatch(ctx, convID, line)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			continue
		}
		fmt.Println(reply)
	}
}
