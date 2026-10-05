package webbundle

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
)

// Attested is the Source that installs what loomux/web's CI built on its
// main branch, proven by a GitHub build-provenance attestation (LOOM-118
// option b, user decision 2026-10-05). The release JSON says what a
// release is; only a verified attestation (verifyAttestation) says it's
// genuine: signed for the repository's ci.yml running on refs/heads/main,
// over the tarball's sha256, from the commit the release names. Which
// release to install is the newest that verifies and is ahead, on main,
// of the bundle served (Orderer): never a step back or sideways.
//
// Everything it reads is public: the token is optional, for GitHub's rate
// limit. Sigstore's public-good trust root comes from its TUF repository,
// cached under cacheDir.
type Attested struct {
	gh       *GitHub
	cacheDir string

	trustMu sync.Mutex
	trusted root.TrustedMaterial
	// checkBundle checks one attestation bundle, as the API returns it,
	// against rel: parsed and verifyAttestation'd against the trust root
	// (tests replace it).
	checkBundle func(raw json.RawMessage, rel *Release) error
}

// NewAttested is a Source for webRepo's attested releases. token may be
// empty.
func NewAttested(webRepo, token, cacheDir string) *Attested {
	a := &Attested{gh: NewGitHub("", webRepo, token), cacheDir: cacheDir}
	a.checkBundle = func(raw json.RawMessage, rel *Release) error {
		trusted, err := a.trustedRoot()
		if err != nil {
			return err
		}
		var b bundle.Bundle
		if err := b.UnmarshalJSON(raw); err != nil {
			return err
		}
		return verifyAttestation(trusted, &b, a.gh.webRepo, rel)
	}
	return a
}

// trustedRoot is Sigstore's public-good trust root, fetched (from its TUF
// repository) on first use and kept fresh in the background; a failed
// fetch is retried on the next call rather than remembered.
func (a *Attested) trustedRoot() (root.TrustedMaterial, error) {
	a.trustMu.Lock()
	defer a.trustMu.Unlock()
	if a.trusted != nil {
		return a.trusted, nil
	}
	opts := tuf.DefaultOptions()
	opts.CachePath = filepath.Join(a.cacheDir, "sigstore-tuf")
	trusted, err := root.NewLiveTrustedRoot(opts)
	if err != nil {
		return nil, fmt.Errorf("webbundle: Sigstore trust root: %w", err)
	}
	a.trusted = trusted
	return trusted, nil
}

type ghReleaseAssets struct {
	TagName     string    `json:"tag_name"`
	Draft       bool      `json:"draft"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"assets"`
}

// maxAttestedCandidates is how many of the newest releases Latest looks
// at for one that verifies.
const maxAttestedCandidates = 5

// Latest is the newest web-<sha> release whose attestation verifies; one
// that doesn't check out is skipped.
func (a *Attested) Latest(ctx context.Context) (*Release, error) {
	var list []ghReleaseAssets
	if err := a.gh.getJSON(ctx, a.gh.api+"/repos/"+a.gh.webRepo+"/releases?per_page=20", &list); err != nil {
		return nil, err
	}
	// Newest first by publication (the API's order isn't guaranteed), and
	// only the few newest: each costs API requests, and an unauthenticated
	// caller has 60 an hour.
	var candidates []ghReleaseAssets
	for _, gr := range list {
		if !gr.Draft && strings.HasPrefix(gr.TagName, "web-") {
			candidates = append(candidates, gr)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].PublishedAt.After(candidates[j].PublishedAt) })
	if len(candidates) > maxAttestedCandidates {
		candidates = candidates[:maxAttestedCandidates]
	}
	var skipped []string
	for _, gr := range candidates {
		rel, err := a.check(ctx, gr)
		if err != nil {
			skipped = append(skipped, err.Error())
			continue
		}
		return rel, nil
	}
	if len(skipped) > 0 {
		return nil, fmt.Errorf("webbundle: no attested release in %s: %s", a.gh.webRepo, strings.Join(skipped, "; "))
	}
	return nil, fmt.Errorf("webbundle: %s has no web releases", a.gh.webRepo)
}

func (a *Attested) check(ctx context.Context, gr ghReleaseAssets) (*Release, error) {
	assets := map[string]string{}
	for _, as := range gr.Assets {
		assets[as.Name] = as.URL
	}
	jsonURL, ok := assets[releaseFile]
	if !ok {
		return nil, fmt.Errorf("%s has no %s", gr.TagName, releaseFile)
	}
	var rel Release
	if err := a.gh.getAsset(ctx, jsonURL, 1<<16, func(r io.Reader) error { return json.NewDecoder(r).Decode(&rel) }); err != nil {
		return nil, fmt.Errorf("%s: %w", gr.TagName, err)
	}
	if err := rel.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", gr.TagName, err)
	}
	if rel.Tag != gr.TagName || rel.Repo != a.gh.webRepo {
		return nil, fmt.Errorf("%s: its %s names %s in %s", gr.TagName, releaseFile, rel.Tag, rel.Repo)
	}
	if rel.tarballURL, ok = assets[rel.Tarball]; !ok {
		return nil, fmt.Errorf("%s has no %s", gr.TagName, rel.Tarball)
	}
	var att struct {
		Attestations []struct {
			Bundle json.RawMessage `json:"bundle"`
		} `json:"attestations"`
	}
	u := a.gh.api + "/repos/" + a.gh.webRepo + "/attestations/sha256:" + url.PathEscape(rel.SHA256)
	if err := a.gh.getJSON(ctx, u, &att); err != nil {
		return nil, fmt.Errorf("%s: attestations: %w", gr.TagName, err)
	}
	var reasons []string
	for _, raw := range att.Attestations {
		if err := a.checkBundle(raw.Bundle, &rel); err != nil {
			reasons = append(reasons, err.Error())
			continue
		}
		return &rel, nil
	}
	if len(reasons) == 0 {
		return nil, fmt.Errorf("%s has no attestation", gr.TagName)
	}
	return nil, fmt.Errorf("%s: %s", gr.TagName, strings.Join(reasons, "; "))
}

// Tarball opens rel's tarball asset.
func (a *Attested) Tarball(ctx context.Context, rel *Release) (io.ReadCloser, error) {
	return a.gh.Tarball(ctx, rel)
}

// Newer reports whether candidate is ahead of base on the repository's
// history (GitHub's compare: "ahead").
func (a *Attested) Newer(ctx context.Context, base, candidate string) (bool, error) {
	if base == "" {
		return true, nil
	}
	if base == candidate {
		return false, nil
	}
	var cmp struct {
		Status string `json:"status"`
	}
	u := a.gh.api + "/repos/" + a.gh.webRepo + "/compare/" + url.PathEscape(base) + "..." + url.PathEscape(candidate)
	if err := a.gh.getJSON(ctx, u, &cmp); err != nil {
		return false, fmt.Errorf("webbundle: compare %s...%s: %w", base[:7], candidate[:7], err)
	}
	return cmp.Status == "ahead", nil
}
