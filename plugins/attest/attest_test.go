package attest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/sigstore/sigstore-go/pkg/verify"

	"github.com/Loomux/server/registry"
)

// LOOM-218: a plugin executable is verified by a pinned digest or a
// Sigstore build-provenance bundle from a trusted signer.

const trustedSigner = "https://github.com/acme/plugin/.github/workflows/release.yml@refs/heads/main"

// statementFor is an in-toto statement about sum with predicate type pt.
func statementFor(t *testing.T, sum, pt string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"_type":         "https://in-toto.io/Statement/v1",
		"subject":       []any{map[string]any{"name": "loomux-plugin-acme", "digest": map[string]string{"sha256": sum}}},
		"predicateType": pt,
		"predicate":     map[string]any{"buildDefinition": map[string]any{"buildType": "https://actions.github.io/buildtypes/workflow/v1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// testVerifier is an Verifier over vs, trusting signers,
// reading bundles as entities (a placeholder file stands beside the
// executable; the entity is what Attest made).
func testVerifier(vs root.TrustedMaterial, entities map[string]verify.SignedEntity, signers ...string) *Verifier {
	return &Verifier{
		digests:     map[string]bool{},
		signers:     signers,
		trustedRoot: func() (root.TrustedMaterial, error) { return vs, nil },
		// What the test CA can satisfy: it issues no SCTs.
		options: []verify.VerifierOption{verify.WithTransparencyLog(1), verify.WithIntegratedTimestamps(1)},
		loadEntity: func(path string) (verify.SignedEntity, error) {
			e, ok := entities[path]
			if !ok {
				return nil, os.ErrNotExist
			}
			return e, nil
		},
	}
}

// executable writes a plugin executable with body, and returns its path
// and sha256.
func executable(t *testing.T, body string) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "loomux-plugin-acme")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	sum, err := fileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, sum
}

func TestVerifier(t *testing.T) {
	ctx := context.Background()
	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	attest := func(identity, issuer string, statement []byte) verify.SignedEntity {
		t.Helper()
		e, err := vs.Attest(identity, issuer, statement)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	// withBundle is an executable with a bundle beside it holding entity
	// (made for the executable's own digest unless entity says otherwise).
	withBundle := func(t *testing.T, entityFor func(sum string) verify.SignedEntity) (string, map[string]verify.SignedEntity) {
		t.Helper()
		path, sum := executable(t, "acme plugin v1")
		if err := os.WriteFile(path+BundleSuffix, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		return path, map[string]verify.SignedEntity{path + BundleSuffix: entityFor(sum)}
	}
	genuine := func(sum string) verify.SignedEntity {
		return attest(trustedSigner, githubActionsIssuer, statementFor(t, sum, slsaProvenanceV1))
	}

	t.Run("signed by a trusted signer", func(t *testing.T) {
		path, entities := withBundle(t, genuine)
		trust, why := testVerifier(vs, entities, trustedSigner).Verify(ctx, path)
		if trust != registry.PluginTrustSignedPrefix+trustedSigner || why != "" {
			t.Errorf("trust = %q (%s)", trust, why)
		}
	})
	t.Run("pinned", func(t *testing.T) {
		path, sum := executable(t, "acme plugin v1")
		v := testVerifier(vs, nil)
		v.digests[sum] = true
		if trust, why := v.Verify(ctx, path); trust != registry.PluginTrustPinnedPrefix+"sha256:"+sum {
			t.Errorf("trust = %q (%s)", trust, why)
		}
	})
	t.Run("tampered after signing", func(t *testing.T) {
		path, entities := withBundle(t, genuine)
		if err := os.WriteFile(path, []byte("acme plugin v1, with something added"), 0o755); err != nil {
			t.Fatal(err)
		}
		trust, why := testVerifier(vs, entities, trustedSigner).Verify(ctx, path)
		if trust != registry.PluginTrustUnsigned || !strings.Contains(why, "doesn't vouch for its executable") {
			t.Errorf("a binary changed after signing: %q (%s)", trust, why)
		}
	})
	unsigned := map[string]func(sum string) verify.SignedEntity{
		"an untrusted signer": func(sum string) verify.SignedEntity {
			return attest("https://github.com/someone/else/.github/workflows/release.yml@refs/heads/main", githubActionsIssuer, statementFor(t, sum, slsaProvenanceV1))
		},
		"the trusted workflow on another ref": func(sum string) verify.SignedEntity {
			return attest(strings.Replace(trustedSigner, "refs/heads/main", "refs/heads/feature", 1), githubActionsIssuer, statementFor(t, sum, slsaProvenanceV1))
		},
		"another issuer": func(sum string) verify.SignedEntity {
			return attest(trustedSigner, "https://accounts.example.com", statementFor(t, sum, slsaProvenanceV1))
		},
		"not build provenance": func(sum string) verify.SignedEntity {
			return attest(trustedSigner, githubActionsIssuer, statementFor(t, sum, "https://example.com/vuln-scan/v1"))
		},
		"another Sigstore": func(sum string) verify.SignedEntity {
			stranger, err := ca.NewVirtualSigstore()
			if err != nil {
				t.Fatal(err)
			}
			e, err := stranger.Attest(trustedSigner, githubActionsIssuer, statementFor(t, sum, slsaProvenanceV1))
			if err != nil {
				t.Fatal(err)
			}
			return e
		},
	}
	for name, entityFor := range unsigned {
		t.Run(name, func(t *testing.T) {
			path, entities := withBundle(t, entityFor)
			if trust, why := testVerifier(vs, entities, trustedSigner).Verify(ctx, path); trust != registry.PluginTrustUnsigned || why == "" {
				t.Errorf("trust = %q (%s), want unsigned with a reason", trust, why)
			}
		})
	}
	t.Run("nothing vouches", func(t *testing.T) {
		path, sum := executable(t, "acme plugin v1")
		trust, why := testVerifier(vs, nil, trustedSigner).Verify(ctx, path)
		if trust != registry.PluginTrustUnsigned || !strings.Contains(why, "sha256:"+sum) || !strings.Contains(why, "LOOMUX_PLUGIN_DIGESTS") || !strings.Contains(why, BundleSuffix) {
			t.Errorf("no bundle, no pin: %q (%s)", trust, why)
		}
	})
	t.Run("no signer trusted", func(t *testing.T) {
		path, entities := withBundle(t, genuine)
		if trust, why := testVerifier(vs, entities).Verify(ctx, path); trust != registry.PluginTrustUnsigned || !strings.Contains(why, "LOOMUX_PLUGIN_SIGNERS") {
			t.Errorf("a bundle, no signers: %q (%s)", trust, why)
		}
	})
}

func TestNew(t *testing.T) {
	good := "sha256:" + strings.Repeat("ab", 32)
	if _, err := New([]string{good, strings.ToUpper(good[7:])}, []string{trustedSigner}, "", t.TempDir()); err == nil {
		t.Error("a digest without sha256: was accepted")
	}
	if v, err := New([]string{"SHA256:" + strings.Repeat("AB", 32)}, []string{trustedSigner}, "", t.TempDir()); err != nil || !v.digests[strings.Repeat("ab", 32)] {
		t.Errorf("an upper-case digest: %v", err)
	}
	for _, bad := range []string{"sha256:abc", "md5:" + strings.Repeat("ab", 16), "sha256:" + strings.Repeat("zz", 32)} {
		if _, err := New([]string{bad}, nil, "", t.TempDir()); err == nil {
			t.Errorf("digest %q accepted", bad)
		}
	}
	for _, bad := range []string{"acme/plugin", "http://github.com/x", "https://github.com/a b"} {
		if _, err := New(nil, []string{bad}, "", t.TempDir()); err == nil {
			t.Errorf("signer %q accepted", bad)
		}
	}
	missing := filepath.Join(t.TempDir(), "trusted_root.json")
	if _, err := New(nil, nil, missing, t.TempDir()); err == nil {
		t.Error("a missing trusted root file was accepted")
	}
}
