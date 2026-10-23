package router

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	pathpkg "path"
	"regexp"
	"strings"
	"unicode"

	"github.com/Loomux/server/registry"
)

// provisioningMarker starts every provisioning recipe, followed by the
// kind and the workspace name — recognisable in a pane, a task's recorded
// command, or an offer.
const provisioningMarker = "# loomux provisioning:"

// provisionedPathPrefix and provisionedRootPrefix mark the lines a
// recipe prints with the directory it resolved and the root it confined
// it to. Each is followed by the run's nonce (LOOM-153), so a line the
// clone's own output puts on the screen can't pass for one: see
// parseProvisionedPath.
const (
	provisionedPathPrefix = "loomux-workspace-path:"
	provisionedRootPrefix = "loomux-workspace-root:"
)

// defaultWorkspaceRoot is where a target with no WorkspaceRoot configured
// gets its workspaces, relative to the login user's $HOME there.
const defaultWorkspaceRoot = "loomux-workspaces"

// WorkspaceNamePattern is what a new workspace's name must match: also
// its directory name. The routing model's schema uses the same pattern
// (llmrouter), so the two can't drift apart.
const WorkspaceNamePattern = `^[a-z0-9][a-z0-9_-]{0,62}$`

var (
	workspaceSlug = regexp.MustCompile(WorkspaceNamePattern)
	// gitRemoteURL accepts https:// and ssh:// URLs and gitRemoteSCP
	// scp-style user@host:path — never ext::, file:// or anything else git
	// would treat as a transport that runs commands or reads the target's
	// own files. The user (if any) and host must each start with a letter
	// or digit, and an scp-style path must not start with a dash, so no
	// part can be read by git or ssh as an option (`-oProxyCommand=…`).
	gitRemoteURL = regexp.MustCompile(`^(https|ssh)://(?:[A-Za-z0-9][A-Za-z0-9._~%+-]*@)?[A-Za-z0-9][A-Za-z0-9.-]*(?::[0-9]+)?(?:/[A-Za-z0-9._~%+@:/-]*)?$`)
	gitRemoteSCP = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*@[A-Za-z0-9][A-Za-z0-9.-]*:[A-Za-z0-9._~/+][A-Za-z0-9._~/+-]*$`)
	// gitRefName is what a worktree's ref and branch may be: a
	// conservative subset of git's ref names that never starts with a
	// dash (no option) and needs no quoting thought; git itself checks
	// the branch again (check-ref-format) when the recipe runs.
	gitRefName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
)

// validRefName is gitRefName without what git refuses in a ref name:
// "..", "//", a component starting with "." or ending in ".lock", a
// trailing "/" or ".".
func validRefName(s string) bool {
	if !gitRefName.MatchString(s) || strings.Contains(s, "..") || strings.Contains(s, "//") ||
		strings.HasSuffix(s, "/") || strings.HasSuffix(s, ".") {
		return false
	}
	for _, part := range strings.Split(s, "/") {
		if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

// worktreeBranch is the branch a git_worktree workspace gets.
func (s ProvisionSpec) worktreeBranch() string {
	if s.Worktree.Branch != "" {
		return s.Worktree.Branch
	}
	return "loomux/" + s.Name
}

// Validate rejects any spec provisioning can't safely act on. Its errors
// are safe to show to the user.
func (s ProvisionSpec) Validate() error {
	if !workspaceSlug.MatchString(s.Name) {
		return fmt.Errorf("workspace name %q must be lowercase letters, digits, dashes and underscores (at most 63, starting with a letter or digit)", s.Name)
	}
	if s.Kind != ProvisionGitWorktree && s.Worktree != (WorktreeSpec{}) {
		return fmt.Errorf("worktree is only used with kind %q", ProvisionGitWorktree)
	}
	switch s.Kind {
	case ProvisionEmpty, ProvisionExistingDir:
		if s.GitRemote != "" {
			return fmt.Errorf("git_remote is only used with kind %q", ProvisionGitClone)
		}
	case ProvisionGitClone:
		if !gitRemoteURL.MatchString(s.GitRemote) && !gitRemoteSCP.MatchString(s.GitRemote) {
			return fmt.Errorf("git_remote %q must be an https://, ssh:// or user@host:path URL", s.GitRemote)
		}
	case ProvisionGitWorktree:
		w := s.Worktree
		switch {
		case s.GitRemote != "":
			return fmt.Errorf("git_remote is only used with kind %q", ProvisionGitClone)
		case !workspaceSlug.MatchString(w.Base):
			return fmt.Errorf("worktree base %q must be the name of a directory under the workspace root: lowercase letters, digits, dashes and underscores", w.Base)
		case w.Base == s.Name:
			return fmt.Errorf("worktree base %q is the workspace's own name: a worktree needs its own directory", w.Base)
		case w.Ref != "" && !validRefName(w.Ref):
			return fmt.Errorf("worktree ref %q must be a branch, tag or commit name (letters, digits, . _ / -)", w.Ref)
		case !validRefName(s.worktreeBranch()):
			return fmt.Errorf("worktree branch %q must be a branch name (letters, digits, . _ / -)", s.worktreeBranch())
		}
	default:
		return fmt.Errorf("provisioning kind must be %q, %q, %q or %q, not %q",
			ProvisionEmpty, ProvisionGitClone, ProvisionExistingDir, ProvisionGitWorktree, s.Kind)
	}
	return nil
}

// provisioningRecipe is the whole of what provisioning runs on a target
// (LOOM-90): a POSIX sh script Loomux builds from a validated spec, every
// value in it quoted. It creates (or clones, or adopts) <root>/<name>,
// resolves that directory with symlinks followed, refuses it unless it is
// still inside the root — so a symlink can't hand an agent ~/.ssh — and
// prints the resolved path and root, tagged with nonce (newRecipeNonce).
// spec must have passed Validate.
func provisioningRecipe(target *registry.Target, spec ProvisionSpec, nonce string) string {
	root := `"$HOME"/` + defaultWorkspaceRoot
	if target.WorkspaceRoot != "" {
		root = shellQuote(target.WorkspaceRoot)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s %s\n", provisioningMarker, spec.Kind, spec.Name)
	b.WriteString("set -eu\n")
	fmt.Fprintf(&b, "root=%s\n", root)
	b.WriteString(`mkdir -p -- "$root"` + "\n")
	b.WriteString(`r=$(cd -- "$root" && pwd -P)` + "\n")
	fmt.Fprintf(&b, "p=\"$r\"/%s\n", shellQuote(spec.Name))
	switch spec.Kind {
	case ProvisionEmpty:
		b.WriteString(`mkdir -p -- "$p"` + "\n")
	case ProvisionGitClone:
		b.WriteString(`if [ -e "$p" ] || [ -L "$p" ]; then echo "loomux: $p already exists" >&2; exit 1; fi` + "\n")
		fmt.Fprintf(&b, "git clone -- %s \"$p\"\n", shellQuote(spec.GitRemote))
	case ProvisionExistingDir:
		b.WriteString(`[ -d "$p" ] || { echo "loomux: no directory $p" >&2; exit 1; }` + "\n")
	case ProvisionGitWorktree:
		// The base is confined like the workspace: a symlink out of the
		// root can't make another directory's repository the source.
		ref := spec.Worktree.Ref
		if ref == "" {
			ref = "HEAD"
		}
		branch := shellQuote(spec.worktreeBranch())
		fmt.Fprintf(&b, "b=\"$r\"/%s\n", shellQuote(spec.Worktree.Base))
		b.WriteString(`[ -d "$b" ] || { echo "loomux: no repository directory $b" >&2; exit 1; }` + "\n")
		b.WriteString(`base=$(cd -- "$b" && pwd -P)` + "\n")
		b.WriteString(`case "$base" in "$r"/*) ;; *) echo "loomux: $b resolves to $base, outside the workspace root $r" >&2; exit 1 ;; esac` + "\n")
		b.WriteString(`git -C "$base" rev-parse --git-dir >/dev/null 2>&1 || { echo "loomux: $base isn't a git repository" >&2; exit 1; }` + "\n")
		b.WriteString(`if [ -e "$p" ] || [ -L "$p" ]; then echo "loomux: $p already exists" >&2; exit 1; fi` + "\n")
		fmt.Fprintf(&b, "git check-ref-format --branch %s >/dev/null || { echo \"loomux: %s isn't a branch name git accepts\" >&2; exit 1; }\n", branch, spec.worktreeBranch())
		fmt.Fprintf(&b, "git -C \"$base\" worktree add -b %s -- \"$p\" %s\n", branch, shellQuote(ref))
	}
	b.WriteString(`real=$(cd -- "$p" && pwd -P)` + "\n")
	b.WriteString(`case "$real" in "$r"/*) ;; *) echo "loomux: $p resolves to $real, outside the workspace root $r" >&2; exit 1 ;; esac` + "\n")
	b.WriteString(`printf '` + provisionedRootPrefix + nonce + `:%s\n' "$r"` + "\n")
	b.WriteString(`printf '` + provisionedPathPrefix + nonce + `:%s\n' "$real"` + "\n")
	return b.String()
}

// workspaceDisplayPath is where a workspace named name will be on target,
// for showing to a person: the configured root, or ~/loomux-workspaces.
// The recipe resolves (and confines) the real directory when it runs.
func workspaceDisplayPath(target *registry.Target, name string) string {
	root := "~/" + defaultWorkspaceRoot
	if target.WorkspaceRoot != "" {
		root = target.WorkspaceRoot
	}
	return root + "/" + name
}

// newRecipeNonce is a fresh, shell-safe nonce for one recipe run.
func newRecipeNonce() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("router: recipe nonce: %v", err))
	}
	return hex.EncodeToString(b)
}

// parseProvisionedPath reads the directory a recipe run tagged nonce
// printed, or "" if it printed none that holds up (LOOM-153). The output
// is the pane's screen, which also shows whatever the clone printed — a
// hostile git server's messages, escape sequences and all — so only lines
// carrying this run's nonce count, the last of each wins (the recipe
// prints them at the very end), and the path must be clean and inside
// the root the recipe printed: the confinement the shell checked is
// checked again here.
func parseProvisionedPath(output, nonce string) string {
	if nonce == "" {
		return ""
	}
	var root, path string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, provisionedRootPrefix+nonce+":"); ok {
			root = v
		}
		if v, ok := strings.CutPrefix(line, provisionedPathPrefix+nonce+":"); ok {
			path = v
		}
	}
	if !strings.HasPrefix(root, "/") || !strings.HasPrefix(path, "/") ||
		pathpkg.Clean(root) != root || pathpkg.Clean(path) != path {
		return ""
	}
	if !strings.HasPrefix(path, strings.TrimSuffix(root, "/")+"/") || path == root {
		return ""
	}
	return path
}

var errNoProvisionedPath = errors.New("provisioning finished without reporting the workspace directory")

// namesRemote says whether message names remote as a whole word — the
// user typed that repository — and not merely contains it (LOOM-152):
// "…/tools-fork" doesn't name "…/tools". Words split on whitespace,
// quotes, brackets, backticks and markdown's * and |, and lose trailing
// punctuation; a trailing "/" or ".git" doesn't make a different
// repository.
func namesRemote(message, remote string) bool {
	want := canonicalRemote(remote)
	if want == "" {
		return false
	}
	words := strings.FieldsFunc(message, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune("\"'`<>()[]{}*|", r)
	})
	for _, w := range words {
		if canonicalRemote(strings.TrimRight(w, ".,;:!?")) == want {
			return true
		}
	}
	return false
}

func canonicalRemote(remote string) string {
	remote = strings.TrimSuffix(remote, "/")
	return strings.TrimSuffix(remote, ".git")
}
