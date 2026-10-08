package registry

import (
	"errors"
	"time"
)

// SSHKey is an SSH key pair Loomux manages for reaching targets
// (LOOM-138, docs/design/target-onboarding.md): generated in-process, or
// imported from the deployment's mounted SSH config. A target uses one
// through Target.SSHKeyRef.
type SSHKey struct {
	ID   string
	Name string
	// Type is the key's algorithm as ssh names it ("ssh-ed25519").
	Type string
	// PublicKey is the authorized_keys line to install on a target.
	PublicKey string
	// Fingerprint is the SHA256 fingerprint as ssh-keygen -l prints it.
	Fingerprint string
	// Origin is SSHKeyOriginGenerated or SSHKeyOriginImported.
	Origin string
	// PrivateKey is the OpenSSH-format private key. Plaintext at this
	// level, like Credential.Value: the store encrypts it at rest. Only
	// GetSSHKey fills it in; nothing else ever reads it back.
	PrivateKey []byte
	CreatedAt  time.Time
}

// SSHKey origins.
const (
	SSHKeyOriginGenerated = "generated"
	SSHKeyOriginImported  = "imported"
)

// ErrNoMasterKey is a secret-holding operation attempted on a store that
// has no master key to encrypt or decrypt with.
var ErrNoMasterKey = errors.New("registry: no master key configured")
