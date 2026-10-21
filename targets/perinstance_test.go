package targets

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Loomux/server/registry"
)

// LOOM-199: managed SSH is per ExecutorOptions, never process-wide. Two
// configurations in use at once, as two app instances in one process
// would use them, each reach a managed target their own way: their own
// proxy, agent and public key file; options without one say it's off.
func TestManagedSSH_PerInstance(t *testing.T) {
	tgt, a := setupManagedArgs(t, "127.0.0.1:1055")
	_, b := setupManagedArgs(t, "10.9.8.7:1080")
	a.KnownHostsDir, b.KnownHostsDir = t.TempDir(), t.TempDir()
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			opts, other := a, b
			if i%2 == 1 {
				opts, other = b, a
			}
			args, err := managedArgsFor(tgt, opts)
			if err != nil {
				t.Errorf("%d: %v", i, err)
				return
			}
			joined := strings.Join(args, "\n")
			proxy, _ := optionValue(args, "ProxyCommand")
			if !strings.Contains(proxy, "'"+opts.Managed.Proxy+"'") || strings.Contains(proxy, other.Managed.Proxy) {
				t.Errorf("%d: ProxyCommand %q, want this instance's proxy %s", i, proxy, opts.Managed.Proxy)
			}
			if agent, _ := optionValue(args, "IdentityAgent"); !strings.HasPrefix(agent, opts.Managed.Agents.dir+"/") {
				t.Errorf("%d: IdentityAgent %q isn't this instance's agent pool %s", i, agent, opts.Managed.Agents.dir)
			}
			if !strings.Contains(joined, opts.Managed.KeysDir+"/") || strings.Contains(joined, other.Managed.KeysDir+"/") {
				t.Errorf("%d: public key file isn't under this instance's keys dir %s: %q", i, opts.Managed.KeysDir, args)
			}
			if !strings.Contains(joined, opts.KnownHostsDir+"/") || strings.Contains(joined, other.KnownHostsDir+"/") {
				t.Errorf("%d: pin isn't under this instance's known_hosts dir %s: %q", i, opts.KnownHostsDir, args)
			}
			if _, err := managedArgsFor(tgt, ExecutorOptions{KnownHostsDir: opts.KnownHostsDir}); !errors.Is(err, errManagedOff) {
				t.Errorf("%d: options without managed SSH = %v, want it off", i, err)
			}
		}()
	}
	wg.Wait()
}

// managedArgsFor is the ssh arguments an executor for t built with o
// adds to its command line.
func managedArgsFor(t *registry.Target, o ExecutorOptions) ([]string, error) {
	opts, err := remoteOptions(t, o.KnownHostsDir, o.Managed)
	if err != nil {
		return nil, err
	}
	e := &RemoteExecutor{}
	for _, opt := range opts {
		opt(e)
	}
	return e.extraArgs, nil
}

// LOOM-199: whether local targets run is per ExecutorOptions too: one
// instance's being off never turns another's off.
func TestLocalTargets_PerInstance(t *testing.T) {
	local := &registry.Target{Name: "box", Kind: registry.TargetKindLocal}
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			off := i%2 == 1
			e, err := NewExecutorWith(local, ExecutorOptions{LocalTargetsOff: off})
			if off != errors.Is(err, ErrLocalTargetsOff) {
				t.Errorf("LocalTargetsOff %v: err = %v", off, err)
			}
			if e != nil {
				e.Close()
			}
		}()
	}
	wg.Wait()
}

// processWideVars are the package-level variables this package may have,
// each either never written after init or a test seam. Instance
// configuration belongs in ExecutorOptions (LOOM-197, LOOM-199): a new
// variable here needs a reason it isn't.
var processWideVars = map[string]string{
	// Fixed at init.
	"ErrLocalTargetsOff": "error",
	"ErrTextTooLarge":    "error",
	"ErrUnreachable":     "error",
	"errManagedOff":      "error",
	"errReadOnlyAgent":   "error",
	"localEnvAllowed":    "constant table",
	"sshFailurePatterns": "constant table",
	"socksProxyCommands": "constant table",
	"relayPath":          "regexp",
	"proxyHostName":      "regexp",
	"keyFileName":        "regexp",
	"sshBanner":          "regexp",
	"sshKeyID":           "regexp",
	"sshKeyName":         "regexp",
	"tmuxSocketName":     "regexp",
	"exitStatusMarker":   "regexp",
	// Shared by nature: guards and per-ControlMaster limits, keyed by
	// paths an instance owns.
	"knownHostsMu": "mutex",
	"opSlots":      "per-ControlMaster semaphores",
	// Test seams, written only by export_test.go or this package's tests.
	"sshConfigFile":          "test seam",
	"sshDir":                 "test seam",
	"deadPaneRecaptureDelay": "test seam",
	"deadPaneRecaptures":     "test seam",
	"statusPendingWait":      "test seam",
	"awaitMarkerTries":       "test seam", // WrapExitStatus's wait bound (LOOM-198)
	// Process-wide still: the tmux socket, set once by SetTmuxSocket.
	// Not LOOM-199's; a follow-up moves it like the rest.
	"TmuxSocket": "process-wide (follow-up)",
}

// LOOM-199: no package-level state holds an instance's configuration —
// the managed SSH setup and the local-target switch are gone, and any
// new variable has to be named in processWideVars.
func TestNoProcessWideInstanceConfig(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, d := range f.Decls {
				if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.VAR {
					for _, s := range g.Specs {
						for _, n := range s.(*ast.ValueSpec).Names {
							found = append(found, n.Name)
						}
					}
				}
			}
		}
	}
	for _, name := range found {
		if _, ok := processWideVars[name]; !ok {
			t.Errorf("package-level var %s: per-instance configuration goes in ExecutorOptions; if it isn't that, add it to processWideVars with why", name)
		}
	}
	for _, gone := range []string{"managed", "managedMu", "LocalTargets"} {
		if slices.Contains(found, gone) {
			t.Errorf("package-level var %s is back (LOOM-199)", gone)
		}
	}
}
