package router

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/Loomux/server/registry"
)

// provisioningMarker starts every provisioning recipe, followed by the
// kind and the workspace name — recognisable in a pane, a task's recorded
// command, or an offer.
const provisioningMarker = "# loomux provisioning:"

// provisionedPathPrefix marks the line a recipe prints with the
// directory it resolved: what the workspace's path becomes.
const provisionedPathPrefix = "loomux-workspace-path:"

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
)

// Validate rejects any spec provisioning can't safely act on. Its errors
// are safe to show to the user.
func (s ProvisionSpec) Validate() error {
	if !workspaceSlug.MatchString(s.Name) {
		return fmt.Errorf("workspace name %q must be lowercase letters, digits, dashes and underscores (at most 63, starting with a letter or digit)", s.Name)
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
	default:
		return fmt.Errorf("provisioning kind must be %q, %q or %q, not %q",
			ProvisionEmpty, ProvisionGitClone, ProvisionExistingDir, s.Kind)
	}
	return nil
}

// provisioningRecipe is the whole of what provisioning runs on a target
// (LOOM-90): a POSIX sh script Loomux builds from a validated spec, every
// value in it quoted. It creates (or clones, or adopts) <root>/<name>,
// resolves that directory with symlinks followed, refuses it unless it is
// still inside the root — so a symlink can't hand an agent ~/.ssh — and
// prints the resolved path. spec must have passed Validate.
func provisioningRecipe(target *registry.Target, spec ProvisionSpec) string {
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
	}
	b.WriteString(`real=$(cd -- "$p" && pwd -P)` + "\n")
	b.WriteString(`case "$real" in "$r"/*) ;; *) echo "loomux: $p resolves to $real, outside the workspace root $r" >&2; exit 1 ;; esac` + "\n")
	b.WriteString(`printf '` + provisionedPathPrefix + `%s\n' "$real"` + "\n")
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

// parseProvisionedPath reads the resolved directory a recipe printed, or
// "" if it printed none (or something that isn't an absolute path).
func parseProvisionedPath(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if p, ok := strings.CutPrefix(line, provisionedPathPrefix); ok && strings.HasPrefix(p, "/") {
			return p
		}
	}
	return ""
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
