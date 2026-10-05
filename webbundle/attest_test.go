package webbundle

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

const ciMain = "https://github.com/loomux/web/.github/workflows/ci.yml@refs/heads/main"

// statement is an in-toto SLSA v1 provenance statement for a tarball
// digest built from commit, shaped as actions/attest-build-provenance
// writes it.
func statement(t *testing.T, digest, commit string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"_type":         "https://in-toto.io/Statement/v1",
		"subject":       []any{map[string]any{"name": "loomux-web-aaaaaaa.tar.gz", "digest": map[string]string{"sha256": digest}}},
		"predicateType": slsaProvenanceV1,
		"predicate": map[string]any{
			"buildDefinition": map[string]any{
				"buildType":            "https://actions.github.io/buildtypes/workflow/v1",
				"externalParameters":   map[string]any{"workflow": map[string]any{"ref": "refs/heads/main", "repository": "https://github.com/loomux/web", "path": ".github/workflows/ci.yml"}},
				"resolvedDependencies": []any{map[string]any{"uri": "git+https://github.com/loomux/web@refs/heads/main", "digest": map[string]string{"gitCommit": commit}}},
			},
			"runDetails": map[string]any{"builder": map[string]any{"id": ciMain}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// virtualOpts are what the test CA can satisfy: it issues no SCTs.
var virtualOpts = []verify.VerifierOption{verify.WithTransparencyLog(1), verify.WithIntegratedTimestamps(1)}

func TestVerifyAttestation(t *testing.T) {
	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	tgz := tarball(t, entry{name: "index.html", body: "attested"})
	rel := release("aaaaaaa", time.Now(), tgz)
	attest := func(identity, issuer string, body []byte) *ca.TestEntity {
		t.Helper()
		e, err := vs.Attest(identity, issuer, body)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}

	if err := verifyAttestation(vs, attest(ciMain, githubActionsIssuer, statement(t, rel.SHA256, rel.Commit)), "loomux/web", rel, virtualOpts...); err != nil {
		t.Fatalf("genuine attestation refused: %v", err)
	}

	other := strings.Repeat("b", 64)
	for name, e := range map[string]*ca.TestEntity{
		// A workflow run for any branch but main: "a pushed branch can
		// publish a release", the hole the pin model closed.
		"branch build":   attest("https://github.com/loomux/web/.github/workflows/ci.yml@refs/heads/feature", githubActionsIssuer, statement(t, rel.SHA256, rel.Commit)),
		"pull request":   attest("https://github.com/loomux/web/.github/workflows/ci.yml@refs/pull/7/merge", githubActionsIssuer, statement(t, rel.SHA256, rel.Commit)),
		"other workflow": attest("https://github.com/loomux/web/.github/workflows/evil.yml@refs/heads/main", githubActionsIssuer, statement(t, rel.SHA256, rel.Commit)),
		"other repo":     attest("https://github.com/someone/web/.github/workflows/ci.yml@refs/heads/main", githubActionsIssuer, statement(t, rel.SHA256, rel.Commit)),
		"other issuer":   attest(ciMain, "https://accounts.example.com", statement(t, rel.SHA256, rel.Commit)),
		"other digest":   attest(ciMain, githubActionsIssuer, statement(t, other, rel.Commit)),
		"other commit":   attest(ciMain, githubActionsIssuer, statement(t, rel.SHA256, strings.Repeat("c", 40))),
	} {
		t.Run(name, func(t *testing.T) {
			if err := verifyAttestation(vs, e, "loomux/web", rel, virtualOpts...); err == nil {
				t.Error("verified")
			}
		})
	}

	// Signed by another Sigstore (not the trusted one): refused.
	stranger, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := stranger.Attest(ciMain, githubActionsIssuer, statement(t, rel.SHA256, rel.Commit))
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyAttestation(vs, foreign, "loomux/web", rel, virtualOpts...); err == nil {
		t.Error("an attestation from an untrusted Sigstore verified")
	}
}
