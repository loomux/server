package agents

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/Loomux/server/router"
)

// claudeAuthDetails reads `claude auth status`'s JSON (LOOM-316):
// authMethod ("claude.ai", "api_key", "oauth_token", "third_party",
// "none"), subscriptionType ("max", "pro") and loginExpiresAt. Only those
// fields are decoded; email, orgId and orgName are never read. Anything
// around the JSON object (a warning line) is ignored.
func claudeAuthDetails(output string) router.AuthDetails {
	start, end := strings.Index(output, "{"), strings.LastIndex(output, "}")
	if start < 0 || end < start {
		return router.AuthDetails{}
	}
	var v struct {
		AuthMethod       string `json:"authMethod"`
		SubscriptionType string `json:"subscriptionType"`
		LoginExpiresAt   string `json:"loginExpiresAt"`
	}
	if err := json.Unmarshal([]byte(output[start:end+1]), &v); err != nil {
		return router.AuthDetails{}
	}
	d := router.AuthDetails{Method: v.AuthMethod, Plan: v.SubscriptionType}
	if t, err := time.Parse(time.RFC3339Nano, v.LoginExpiresAt); err == nil {
		d.ExpiresAt = t.UTC()
	}
	return d
}

// codexLoggedInUsing is `codex login status`'s "Logged in using …" line;
// the method is named by its first words only, since after an API key it
// prints part of the key.
var codexLoggedInUsing = regexp.MustCompile(`(?im)^\s*logged in using\s+(chatgpt|an api key|api key|an access token|access token)\b`)

// codexAuthDetails reads the sign-in method from `codex login status`
// (LOOM-316): "chatgpt", "api_key" or "access_token". Codex shows no plan
// or expiry there.
func codexAuthDetails(output string) router.AuthDetails {
	m := codexLoggedInUsing.FindStringSubmatch(output)
	if m == nil {
		return router.AuthDetails{}
	}
	switch method := strings.ToLower(m[1]); {
	case method == "chatgpt":
		return router.AuthDetails{Method: "chatgpt"}
	case strings.HasSuffix(method, "api key"):
		return router.AuthDetails{Method: "api_key"}
	default:
		return router.AuthDetails{Method: "access_token"}
	}
}
