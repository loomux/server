// Package attest verifies plugin executables for loomuxd (LOOM-218): the
// plugins.Verifier the catalog asks about plugins from the plugin
// directory. Its own package so plugin binaries, which import package
// plugins, don't carry sigstore-go.
package attest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"

	"github.com/Loomux/server/registry"
)

// Verifying a plugin's executable (LOOM-218, design §1.6). A plugin from
// the plugin directory runs as loomuxd's own user, so what runs must be
// what someone vouched for: its executable's sha256 is either one the
// operator pinned (LOOMUX_PLUGIN_DIGESTS), or the subject of a Sigstore
// bundle beside it (<executable>.sigstore.json, as
// actions/attest-build-provenance or `gh attestation download` makes
// one) that verifies: SLSA v1 build provenance, signed by a certificate
// GitHub Actions issued to one of the identities the operator trusts
// (LOOMUX_PLUGIN_SIGNERS, workflow URIs such as
// https://github.com/acme/plugin/.github/workflows/release.yml@refs/heads/main),
// against Sigstore's trust root. That root is a file the operator gives
// (LOOMUX_PLUGIN_TRUSTED_ROOT, for a pinned or offline one), or else
// Sigstore's public-good one, from its TUF repository: sigstore-go's
// client authenticates every update against the root it embeds, as the
// LOOM-118 web-bundle verifier does, and it is fetched only when a
// bundle has to be checked. The manifest beside the executable needs no
// signature of its own: the handshake refuses a plugin whose manifest
// differs from the one the (verified) executable describes.

// BundleSuffix names the Sigstore bundle beside a plugin's executable.
const BundleSuffix = ".sigstore.json"

// GitHub Actions' OIDC issuer, and the predicate build provenance has.
const (
	githubActionsIssuer = "https://token.actions.githubusercontent.com"
	slsaProvenanceV1    = "https://slsa.dev/provenance/v1"
)

// Verifier is the Catalog's plugins.Verifier for LOOM-218.
type Verifier struct {
	digests map[string]bool
	signers []string
	// trustedRoot is the Sigstore trust root, loaded once.
	trustedRoot func() (root.TrustedMaterial, error)
	// options are the verifier's requirements (tests: what their CA
	// can satisfy); loadEntity reads a bundle (tests: a test entity).
	options    []verify.VerifierOption
	loadEntity func(path string) (verify.SignedEntity, error)
}

// New is a verifier pinning digests ("sha256:<hex>")
// and trusting signers (certificate SANs issued by GitHub Actions),
// against the trust root in trustedRootFile, or Sigstore's public-good
// one (cached under cacheDir) when that is "".
func New(digests, signers []string, trustedRootFile, cacheDir string) (*Verifier, error) {
	v := &Verifier{digests: map[string]bool{}}
	for _, d := range digests {
		hexSum, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(d)), "sha256:")
		if b, err := hex.DecodeString(hexSum); !ok || err != nil || len(b) != sha256.Size {
			return nil, fmt.Errorf("plugins: digest %q: want sha256:<64 hex digits>", d)
		}
		v.digests[hexSum] = true
	}
	for _, s := range signers {
		s = strings.TrimSpace(s)
		if !strings.HasPrefix(s, "https://") || strings.ContainsAny(s, " \t") {
			return nil, fmt.Errorf("plugins: signer %q: want the signing workflow's URI, https://github.com/<owner>/<repo>/.github/workflows/<file>@<ref>", s)
		}
		v.signers = append(v.signers, s)
	}
	if trustedRootFile != "" {
		trusted, err := root.NewTrustedRootFromPath(trustedRootFile)
		if err != nil {
			return nil, fmt.Errorf("plugins: trusted root %s: %w", trustedRootFile, err)
		}
		v.trustedRoot = func() (root.TrustedMaterial, error) { return trusted, nil }
	} else {
		v.trustedRoot = publicGoodRoot(filepath.Join(cacheDir, "sigstore-tuf"))
	}
	v.options = []verify.VerifierOption{
		verify.WithSignedCertificateTimestamps(1),
		verify.WithTransparencyLog(1),
		verify.WithIntegratedTimestamps(1),
	}
	v.loadEntity = func(path string) (verify.SignedEntity, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var b bundle.Bundle
		if err := b.UnmarshalJSON(data); err != nil {
			return nil, err
		}
		return &b, nil
	}
	return v, nil
}

// publicGoodRoot loads Sigstore's public-good trust root on first use;
// a failed fetch is tried again next time rather than remembered.
func publicGoodRoot(cachePath string) func() (root.TrustedMaterial, error) {
	var mu sync.Mutex
	var trusted root.TrustedMaterial
	return func() (root.TrustedMaterial, error) {
		mu.Lock()
		defer mu.Unlock()
		if trusted != nil {
			return trusted, nil
		}
		opts := tuf.DefaultOptions()
		opts.CachePath = cachePath
		t, err := root.NewLiveTrustedRoot(opts)
		if err != nil {
			return nil, fmt.Errorf("Sigstore trust root: %w", err)
		}
		trusted = t
		return trusted, nil
	}
}

// Verify is the trust of the executable at path: pinned:sha256:<hex> or
// signed:<signer> when something vouches for it, else
// registry.PluginTrustUnsigned and why.
func (v *Verifier) Verify(ctx context.Context, path string) (trust, why string) {
	sum, err := fileSHA256(path)
	if err != nil {
		return registry.PluginTrustUnsigned, "its executable can't be read: " + err.Error()
	}
	if v.digests[sum] {
		return registry.PluginTrustPinnedPrefix + "sha256:" + sum, ""
	}
	bundlePath := path + BundleSuffix
	if _, err := os.Stat(bundlePath); errors.Is(err, os.ErrNotExist) {
		return registry.PluginTrustUnsigned, fmt.Sprintf("its executable (sha256:%s) isn't in LOOMUX_PLUGIN_DIGESTS and has no %s beside it", sum, filepath.Base(bundlePath))
	}
	if len(v.signers) == 0 {
		return registry.PluginTrustUnsigned, fmt.Sprintf("it has a Sigstore bundle, but LOOMUX_PLUGIN_SIGNERS names no identity to trust (its executable is sha256:%s)", sum)
	}
	signer, err := v.verifyBundle(bundlePath, sum)
	if err != nil {
		return registry.PluginTrustUnsigned, fmt.Sprintf("its Sigstore bundle doesn't vouch for its executable (sha256:%s): %v", sum, err)
	}
	return registry.PluginTrustSignedPrefix + signer, ""
}

// verifyBundle is the trusted signer whose attestation in the bundle at
// path proves sum, a SLSA v1 build provenance.
func (v *Verifier) verifyBundle(path, sum string) (string, error) {
	entity, err := v.loadEntity(path)
	if err != nil {
		return "", err
	}
	trusted, err := v.trustedRoot()
	if err != nil {
		return "", err
	}
	verifier, err := verify.NewVerifier(trusted, v.options...)
	if err != nil {
		return "", err
	}
	digest, _ := hex.DecodeString(sum)
	var last error
	for _, signer := range v.signers {
		id, err := signerIdentity(signer)
		if err != nil {
			return "", err
		}
		res, err := verifier.Verify(entity, verify.NewPolicy(verify.WithArtifactDigest("sha256", digest), verify.WithCertificateIdentity(id)))
		if err != nil {
			last = err
			continue
		}
		if res.Statement == nil || res.Statement.GetPredicateType() != slsaProvenanceV1 {
			return "", errors.New("it isn't SLSA v1 build provenance")
		}
		return signer, nil
	}
	return "", last
}

// signerIdentity is a certificate GitHub Actions issued to signer, the
// signing workflow's URI (its SAN), exactly.
func signerIdentity(signer string) (verify.CertificateIdentity, error) {
	san, err := verify.NewSANMatcher(signer, "")
	if err != nil {
		return verify.CertificateIdentity{}, err
	}
	issuer, err := verify.NewIssuerMatcher(githubActionsIssuer, "")
	if err != nil {
		return verify.CertificateIdentity{}, err
	}
	return verify.NewCertificateIdentity(san, issuer, certificate.Extensions{})
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
