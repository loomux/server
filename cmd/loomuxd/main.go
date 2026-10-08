// Command loomuxd is the Loomux server process: a thin wrapper around
// app.Build's fully wired domain layer and api.NewServer's client-facing
// HTTP surface (design spec §9, §10 axis 1, LOOM-9). Default mode starts
// the HTTP server (plain HTTP — the deployment model is a reverse proxy
// in front terminating TLS, see api/README.md); -message dispatches one
// message directly, bypassing HTTP/auth entirely, for local debugging;
// -hash-password is a standalone utility for generating
// LOOMUX_AUTH_PASSWORD_HASH.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/app"
	"github.com/Loomux/server/targets"
	"github.com/Loomux/server/version"
	"github.com/Loomux/server/webbundle"
)

func main() {
	// ssh's ProxyCommand for a managed target (LOOM-138): relay stdin and
	// stdout to HOST:PORT through the SOCKS5 proxy, then exit. Not a flag
	// a person runs, so not in -help.
	if len(os.Args) == 5 && os.Args[1] == targets.RelayFlag {
		if err := targets.RelaySOCKS5(os.Args[2], os.Args[3], os.Args[4], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	showVersion := flag.Bool("version", false, "print the server version and exit")
	hashPassword := flag.Bool("hash-password", false, "read a password from stdin, print its bcrypt hash (for LOOMUX_AUTH_PASSWORD_HASH), and exit")
	message := flag.String("message", "", "dispatch a single message directly (bypassing HTTP/auth) and exit, for local debugging")
	conversation := flag.String("conversation", "", "conversation ID to use with -message (default: a freshly generated one)")
	workspaceHint := flag.String("workspace-hint", "", "optional advisory workspace ID to use with -message (LOOM-46; see api/README.md)")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Version)
		return
	}

	if *hashPassword {
		runHashPassword()
		return
	}

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

	if *message != "" {
		convID := *conversation
		if convID == "" {
			convID = uuid.NewString()
		}
		reply, err := loomux.Dispatch(ctx, convID, *message, *workspaceHint)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(reply)
		return
	}

	runServer(ctx, loomux)
}

// runHashPassword reads one line (the plaintext password) from stdin and
// prints its bcrypt hash to stdout — e.g.
// `echo -n 'my password' | loomuxd -hash-password`. A standalone utility
// mode: it never touches app.LoadConfig/app.Build (no DB, no router
// model needed just to hash a password).
func runHashPassword() {
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		fmt.Fprintln(os.Stderr, "loomuxd: could not read a password from stdin")
		os.Exit(1)
	}
	hash, err := api.HashPassword(scanner.Text())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(hash)
}

// runServer starts the client-facing HTTP API and blocks until ctx is
// cancelled (SIGINT/SIGTERM), then shuts down gracefully.
func runServer(ctx context.Context, loomux *app.App) {
	apiCfg, err := api.LoadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	opts := []api.Option{
		api.WithSessionTTL(apiCfg.SessionTTL),
		api.WithLocalTargets(targets.LocalTargets), // set by app.Build from LOOMUX_LOCAL_TARGETS
		api.WithTargetProber(loomux),
		api.WithWorkspaceManager(loomux),
		api.WithTaskCanceller(loomux),
		api.WithAgentTypes(loomux.AgentTypeNames()),
		api.WithTaskTurns(loomux.Store()),
		api.WithEvents(loomux.Store()),
		api.WithHostKeyPinning(targets.ScanHostKey, loomux.Store()),
		api.WithHealthChecker(loomux.HealthChecker()),
		api.WithCredentials(loomux.Store()),
		api.WithSSHKeys(loomux.Store()),
		api.WithSSHKeyDropper(loomux.DropSSHKey),
		api.WithSSHMigration(targets.ResolveSSHConfig),
	}
	if apiCfg.StaticDir != "" {
		opts = append(opts, webOption(apiCfg))
	}

	server := api.NewServer(loomux.Dispatches(), loomux.Store(), loomux.Store(), loomux.Store(), loomux.Store(), loomux.Store(), loomux.Store(), apiCfg.PasswordHash, opts...)
	httpServer := newHTTPServer(apiCfg.Addr, server)

	var metricsServer *http.Server
	if apiCfg.MetricsAddr != "" {
		metricsServer = newHTTPServer(apiCfg.MetricsAddr, loomux.Metrics().Handler())
		go func() {
			fmt.Fprintf(os.Stderr, "loomuxd: metrics listening on %s\n", apiCfg.MetricsAddr)
			if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fmt.Fprintln(os.Stderr, err)
			}
		}()
	}

	go func() {
		<-ctx.Done()
		// Dispatch jobs first (LOOM-80): they get the drain time to finish,
		// the rest are marked interrupted, and a blocking request waiting
		// on one gets its answer before the HTTP server stops.
		if err := loomux.DrainDispatches(); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
		if metricsServer != nil {
			_ = metricsServer.Shutdown(shutdownCtx)
		}
	}()

	fmt.Fprintf(os.Stderr, "loomuxd: listening on %s\n", apiCfg.Addr)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// webOption serves the web client from apiCfg.StaticDir, updatable in
// place (LOOM-118) when a bundles directory is configured. A bundle
// manager that can't start (an unreadable bundle, an unwritable
// directory) is no reason to stay down: the image's bundle is served
// without updates.
func webOption(apiCfg api.Config) api.Option {
	if apiCfg.WebBundlesDir == "" {
		return api.WithStaticDir(apiCfg.StaticDir)
	}
	var source webbundle.Source
	switch apiCfg.WebUpdates {
	case "attested":
		source = webbundle.NewAttested(apiCfg.WebReleasesRepo, apiCfg.WebReleasesToken, apiCfg.WebBundlesDir)
	case "pinned":
		source = webbundle.NewGitHub(apiCfg.WebPinRepo, apiCfg.WebReleasesRepo, apiCfg.WebReleasesToken)
	}
	web, err := webbundle.New(apiCfg.StaticDir, apiCfg.WebBundlesDir, source)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loomuxd: web updates off, serving the image's web client: %v\n", err)
		return api.WithStaticDir(apiCfg.StaticDir)
	}
	fmt.Fprintf(os.Stderr, "loomuxd: serving web client %s from %s (updates: %s)\n",
		web.Current(), web.Root(), apiCfg.WebUpdates)
	return api.WithWebBundles(web)
}
