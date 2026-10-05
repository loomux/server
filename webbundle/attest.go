package webbundle

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// githubActionsIssuer is the OIDC issuer of GitHub Actions' signing
// certificates.
const githubActionsIssuer = "https://token.actions.githubusercontent.com"

// slsaProvenanceV1 is the predicate type actions/attest-build-provenance
// signs.
const slsaProvenanceV1 = "https://slsa.dev/provenance/v1"

// verifyAttestation checks that entity, a signed build-provenance
// attestation (LOOM-118 option b), proves rel was built by repo's CI on
// its main branch (LOOM-118, user decision 2026-10-05):
//
//   - it verifies against trusted (Sigstore's public-good roots in
//     production): certificate chain, transparency log, timestamp;
//   - the signing certificate was issued to repo's workflow
//     .github/workflows/ci.yml running for refs/heads/main, by GitHub
//     Actions' OIDC issuer: a workflow on any other branch, a pull
//     request, or another workflow file can't produce one;
//   - its subject is the tarball's sha256, rel.SHA256 (which the
//     Manager then checks the download against);
//   - the provenance's source commit is rel.Commit, so a genuine
//     attestation can't be paired with another commit's release JSON.
func verifyAttestation(trusted root.TrustedMaterial, entity verify.SignedEntity, repo string, rel *Release, opts ...verify.VerifierOption) error {
	if !sha256RE.MatchString(rel.SHA256) || !commitRE.MatchString(rel.Commit) {
		return errors.New("webbundle: release has no valid digest or commit to check")
	}
	digest, _ := hex.DecodeString(rel.SHA256)
	if len(opts) == 0 {
		opts = []verify.VerifierOption{
			verify.WithSignedCertificateTimestamps(1),
			verify.WithTransparencyLog(1),
			verify.WithIntegratedTimestamps(1),
		}
	}
	v, err := verify.NewVerifier(trusted, opts...)
	if err != nil {
		return fmt.Errorf("webbundle: attestation verifier: %w", err)
	}
	san := "^https://github\\.com/" + regexp.QuoteMeta(repo) + "/\\.github/workflows/ci\\.yml@refs/heads/main$"
	id, err := verify.NewShortCertificateIdentity(githubActionsIssuer, "", "", san)
	if err != nil {
		return fmt.Errorf("webbundle: attestation identity: %w", err)
	}
	res, err := v.Verify(entity, verify.NewPolicy(verify.WithArtifactDigest("sha256", digest), verify.WithCertificateIdentity(id)))
	if err != nil {
		return fmt.Errorf("webbundle: attestation for %s doesn't verify: %w", rel.Tag, err)
	}
	if res.Statement == nil || res.Statement.GetPredicateType() != slsaProvenanceV1 {
		return fmt.Errorf("webbundle: attestation for %s isn't SLSA v1 build provenance", rel.Tag)
	}
	commit, err := provenanceCommit(res.Statement.GetPredicate().AsMap())
	if err != nil {
		return fmt.Errorf("webbundle: attestation for %s: %w", rel.Tag, err)
	}
	if commit != rel.Commit {
		return fmt.Errorf("webbundle: attestation for %s is for commit %s, not %s", rel.Tag, commit, rel.Commit)
	}
	return nil
}

// provenanceCommit is the git commit a SLSA v1 provenance predicate says
// the build ran from: buildDefinition.resolvedDependencies[].digest.gitCommit,
// as actions/attest-build-provenance records it. Exactly one is expected.
func provenanceCommit(predicate map[string]any) (string, error) {
	b, err := json.Marshal(predicate)
	if err != nil {
		return "", err
	}
	var p struct {
		BuildDefinition struct {
			ResolvedDependencies []struct {
				Digest struct {
					GitCommit string `json:"gitCommit"`
				} `json:"digest"`
			} `json:"resolvedDependencies"`
		} `json:"buildDefinition"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return "", err
	}
	var commits []string
	for _, d := range p.BuildDefinition.ResolvedDependencies {
		if d.Digest.GitCommit != "" {
			commits = append(commits, d.Digest.GitCommit)
		}
	}
	if len(commits) != 1 {
		return "", fmt.Errorf("provenance names %d source commits, want 1", len(commits))
	}
	return commits[0], nil
}
