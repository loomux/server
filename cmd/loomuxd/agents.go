package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"
)

// `loomuxd agents` (LOOM-316) is a small client of the agent sign-in API:
// which agent CLIs are signed in on which target, and opening one's
// sign-in to attach to. It talks to a running server over HTTP and never
// opens the database: app.Build starts the server's sweepers and plugins,
// which a second process must not run.

const agentsUsage = `usage:
  loomuxd agents status [-target NAME|ID] [-refresh]
  loomuxd agents sign-in -target NAME|ID -agent TYPE [-restart]
  loomuxd agents sign-in -close -target NAME|ID -agent TYPE

LOOMUX_URL is the server (default http://127.0.0.1:8080); LOOMUX_TOKEN a
session token from POST /api/v1/login.
`

const defaultLoomuxURL = "http://127.0.0.1:8080"

// agentsClient calls the Loomux API with a session token.
type agentsClient struct {
	base  string
	token string
	http  *http.Client
}

// apiError is an error body the API returned.
type apiError struct {
	Status  int
	Message string `json:"error"`
	Code    string `json:"code"`
}

func (e *apiError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("the server answered %d", e.Status)
	}
	return fmt.Sprintf("%s (%s, %d)", e.Message, e.Code, e.Status)
}

// do sends method path with body as JSON (none when nil) and decodes the
// response into out (ignored when nil or 204).
func (c *agentsClient) do(method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		e := &apiError{Status: resp.StatusCode}
		_ = json.Unmarshal(data, e)
		return e
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.Unmarshal(data, out)
}

type agentsTarget struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type agentsAttach struct {
	Via     string `json:"via"`
	Command string `json:"command"`
}

type agentsAgent struct {
	AgentType  string    `json:"agent_type"`
	Available  bool      `json:"available"`
	Version    string    `json:"version"`
	AuthStatus string    `json:"auth_status"`
	CheckedAt  time.Time `json:"checked_at"`
	Auth       *struct {
		State     string     `json:"state"`
		Method    string     `json:"method"`
		Plan      string     `json:"plan"`
		ExpiresAt *time.Time `json:"expires_at"`
		Problem   *struct {
			Kind string `json:"kind"`
		} `json:"problem"`
		RateLimit *struct {
			Resets string `json:"resets"`
		} `json:"rate_limit"`
		SignIn *struct {
			TmuxSession    string         `json:"tmux_session"`
			AttachCommand  string         `json:"attach_command"`
			AttachCommands []agentsAttach `json:"attach_commands"`
		} `json:"sign_in"`
	} `json:"auth"`
}

// state is the agent's one word for the table.
func (a agentsAgent) state() string {
	switch {
	case !a.Available:
		return "not installed"
	case a.Auth == nil || a.Auth.State == "":
		return "-"
	}
	return a.Auth.State
}

// attachLines are the ways to attach to the agent's open sign-in, the
// plain tmux command on the target last; none when none is open.
func (a agentsAgent) attachLines() []string {
	if a.Auth == nil || a.Auth.SignIn == nil {
		return nil
	}
	var lines []string
	for _, c := range a.Auth.SignIn.AttachCommands {
		lines = append(lines, c.Via+": "+c.Command)
	}
	return append(lines, "on the target: "+a.Auth.SignIn.AttachCommand)
}

// runAgents runs `loomuxd agents args…`, reading LOOMUX_URL and
// LOOMUX_TOKEN through getenv, and returns the exit status: 0, 1 for a
// failed call, 2 for a usage error.
func runAgents(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "status" && args[0] != "sign-in") {
		fmt.Fprint(stderr, agentsUsage)
		return 2
	}
	fs := flag.NewFlagSet("loomuxd agents "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	target := fs.String("target", "", "the target, by name or ID")
	refresh := fs.Bool("refresh", false, "status: probe the targets now instead of showing their last probe")
	agent := fs.String("agent", "", "sign-in: the agent type (claude-code, codex)")
	restart := fs.Bool("restart", false, "sign-in: replace an open sign-in, or sign a signed-in agent in again")
	closeIt := fs.Bool("close", false, "sign-in: close the agent's sign-in session instead")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	token := getenv("LOOMUX_TOKEN")
	if token == "" {
		fmt.Fprintln(stderr, "loomuxd agents: LOOMUX_TOKEN is not set: get a session token with POST /api/v1/login")
		return 2
	}
	base := strings.TrimRight(getenv("LOOMUX_URL"), "/")
	if base == "" {
		base = defaultLoomuxURL
	}
	c := &agentsClient{base: base, token: token, http: &http.Client{Timeout: 2 * time.Minute}}

	var err error
	if args[0] == "status" {
		err = agentsStatus(c, *target, *refresh, stdout)
	} else {
		if *target == "" || *agent == "" {
			fmt.Fprintln(stderr, "loomuxd agents sign-in: -target and -agent are required")
			return 2
		}
		err = agentsSignIn(c, *target, *agent, *restart, *closeIt, stdout)
	}
	if err != nil {
		fmt.Fprintln(stderr, "loomuxd agents:", err)
		return 1
	}
	return 0
}

// targetsMatching lists the server's targets, only the one named or with
// that ID when want is set (none such is an error).
func targetsMatching(c *agentsClient, want string) ([]agentsTarget, error) {
	var list struct {
		Targets []agentsTarget `json:"targets"`
	}
	if err := c.do(http.MethodGet, "/api/v1/targets", nil, &list); err != nil {
		return nil, err
	}
	if want == "" {
		return list.Targets, nil
	}
	for _, t := range list.Targets {
		if t.ID == want || t.Name == want {
			return []agentsTarget{t}, nil
		}
	}
	return nil, fmt.Errorf("no target named %q", want)
}

func agentsStatus(c *agentsClient, target string, refresh bool, stdout io.Writer) error {
	list, err := targetsMatching(c, target)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TARGET\tAGENT\tSTATE\tMETHOD\tPLAN\tEXPIRES\tCHECKED\tNOTE")
	type open struct {
		target, agent string
		lines         []string
	}
	var opens []open
	for _, t := range list {
		var resp struct {
			Agents []agentsAgent `json:"agents"`
		}
		path := "/api/v1/targets/" + url.PathEscape(t.ID) + "/agents"
		method := http.MethodGet
		if refresh {
			path, method = "/api/v1/targets/"+url.PathEscape(t.ID)+"/probe", http.MethodPost
		}
		if err := c.do(method, path, nil, &resp); err != nil {
			fmt.Fprintf(tw, "%s\t-\t%s\t\t\t\t\t\n", t.Name, "error: "+err.Error())
			continue
		}
		for _, a := range resp.Agents {
			method, plan, expires, note := "", "", "", ""
			if a.Auth != nil {
				method, plan = a.Auth.Method, a.Auth.Plan
				if a.Auth.ExpiresAt != nil {
					expires = a.Auth.ExpiresAt.Local().Format("2006-01-02")
				}
				switch {
				case a.Auth.RateLimit != nil && a.Auth.RateLimit.Resets != "":
					note = "resets " + a.Auth.RateLimit.Resets
				case a.Auth.Problem != nil:
					note = "a turn saw: " + a.Auth.Problem.Kind
				}
				if a.Auth.SignIn != nil {
					note = strings.TrimPrefix(note+"; sign-in open", "; ")
				}
			}
			checked := ""
			if !a.CheckedAt.IsZero() {
				checked = a.CheckedAt.Local().Format("01-02 15:04")
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", t.Name, a.AgentType, a.state(),
				dash(method), dash(plan), dash(expires), dash(checked), note)
			if lines := a.attachLines(); lines != nil {
				opens = append(opens, open{t.Name, a.AgentType, lines})
			}
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, o := range opens {
		fmt.Fprintf(stdout, "\nSign-in open for %s on %s; attach and finish the login:\n", o.agent, o.target)
		for _, l := range o.lines {
			fmt.Fprintln(stdout, "  "+l)
		}
	}
	return nil
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func agentsSignIn(c *agentsClient, target, agent string, restart, closeIt bool, stdout io.Writer) error {
	list, err := targetsMatching(c, target)
	if err != nil {
		return err
	}
	t := list[0]
	path := "/api/v1/targets/" + url.PathEscape(t.ID) + "/agents/" + url.PathEscape(agent) + "/sign-in"
	if closeIt {
		if err := c.do(http.MethodDelete, path, nil, nil); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "The sign-in session for %s on %s is closed.\n", agent, t.Name)
		return nil
	}
	var resp struct {
		Agent  agentsAgent `json:"agent"`
		Opened bool        `json:"opened"`
		Reused bool        `json:"reused"`
	}
	if err := c.do(http.MethodPost, path, map[string]bool{"restart": restart}, &resp); err != nil {
		return err
	}
	lines := resp.Agent.attachLines()
	if lines == nil {
		how := ""
		if resp.Agent.Auth != nil && resp.Agent.Auth.Method != "" {
			how = " (" + resp.Agent.Auth.Method + ")"
		}
		fmt.Fprintf(stdout, "%s on %s is already signed in%s: nothing to open. Use -restart to sign in again.\n",
			agent, t.Name, how)
		return nil
	}
	verb := "Loomux opened"
	if resp.Reused {
		verb = "Already open:"
	}
	fmt.Fprintf(stdout, "%s the sign-in for %s on %s. Attach and finish the login there:\n", verb, agent, t.Name)
	for _, l := range lines {
		fmt.Fprintln(stdout, "  "+l)
	}
	fmt.Fprintf(stdout, "Then check: loomuxd agents status -target %s -refresh\n", t.Name)
	return nil
}
