package registry

import "time"

// Router model tiers (LOOM-185, docs/design/router-settings.md).
const (
	RouterTierPrimary    = "primary"
	RouterTierEscalation = "escalation"
)

// RouterTier is a router model tier's configuration stored through
// Settings, overriding the environment's for that tier (LOOM-185).
type RouterTier struct {
	// Tier is RouterTierPrimary or RouterTierEscalation: the row's id.
	Tier     string
	Provider string
	BaseURL  string
	Model    string
	// APIKey is plaintext at this level, like SSHKey.PrivateKey: the
	// store encrypts it at rest. Only ListRouterTiers fills it in.
	APIKey string
	// KeyFingerprint and KeyLast4 identify the key without revealing it.
	KeyFingerprint string
	KeyLast4       string
	SetAt          time.Time
}

// Router settings audit actions.
const (
	RouterSettingsSet   = "set"
	RouterSettingsClear = "clear"
)

// RouterSettingsChange is one audit entry for a router tier change:
// which fields changed, never their values.
type RouterSettingsChange struct {
	ID     string
	Tier   string
	Action string
	// Fields are the names of the fields that changed ("provider",
	// "base_url", "model", "api_key").
	Fields    []string
	Actor     string
	CreatedAt time.Time
}
