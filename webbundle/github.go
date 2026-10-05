package webbundle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// GitHub is the Source that serves what loomux/server's main branch pins.
//
// The trust anchor is the pin, not the release: deploy/web-ref (a
// loomux/web commit) and deploy/web-sha256 (its tarball's digest) change
// only through a reviewed PR to loomux/server, the same pin the image
// build checks (#179). The release web-<short> named by the pin supplies
// the bytes, and they must hash to the pinned digest. Nothing about the
// release itself — who created it, its tag, its JSON, its date — can make
// anything else install, since anyone able to push to loomux/web can
// publish a release that looks genuine.
//
// The token needs Contents: read on both repositories (the pin's and the
// releases'). It never leaves the API host.
type GitHub struct {
	api     string
	pinRepo string
	pinRef  string
	webRepo string
	token   string
	client  *http.Client
}

// NewGitHub is a Source serving what pinRepo's main pins, from webRepo's
// releases, authenticating with token.
func NewGitHub(pinRepo, webRepo, token string) *GitHub {
	return &GitHub{api: "https://api.github.com", pinRepo: pinRepo, pinRef: "main", webRepo: webRepo, token: token,
		client: &http.Client{Timeout: 60 * time.Second, CheckRedirect: dropAuthOffHost}}
}

// dropAuthOffHost keeps the token on the API host. An asset download
// redirects to storage elsewhere; Go's client drops Authorization only
// for another domain, not another port, so this doesn't rely on it.
func dropAuthOffHost(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("webbundle: too many redirects")
	}
	if req.URL.Host != via[0].URL.Host {
		req.Header.Del("Authorization")
	}
	return nil
}

// Latest is the release the pin names, with the pinned digest as its
// SHA256.
func (g *GitHub) Latest(ctx context.Context) (*Release, error) {
	ref, err := g.pinFile(ctx, "deploy/web-ref")
	if err != nil {
		return nil, err
	}
	digest, err := g.pinFile(ctx, "deploy/web-sha256")
	if err != nil {
		return nil, err
	}
	if !commitRE.MatchString(ref) {
		return nil, fmt.Errorf("webbundle: %s pins web-ref %q, not a full commit sha", g.pinRepo, ref)
	}
	if !sha256RE.MatchString(digest) {
		return nil, fmt.Errorf("webbundle: %s pins web-sha256 %q, not a sha256", g.pinRepo, digest)
	}
	tag := "web-" + ref[:7]

	var gr struct {
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"assets"`
	}
	if err := g.getJSON(ctx, g.api+"/repos/"+g.webRepo+"/releases/tags/"+url.PathEscape(tag), &gr); err != nil {
		return nil, err
	}
	assets := map[string]string{}
	for _, a := range gr.Assets {
		assets[a.Name] = a.URL
	}
	jsonURL, ok := assets[releaseFile]
	if !ok {
		return nil, fmt.Errorf("webbundle: %s has no %s", tag, releaseFile)
	}
	var rel Release
	if err := g.getAsset(ctx, jsonURL, 1<<16, func(r io.Reader) error { return json.NewDecoder(r).Decode(&rel) }); err != nil {
		return nil, fmt.Errorf("webbundle: %s: %w", tag, err)
	}
	if err := rel.Validate(); err != nil {
		return nil, err
	}
	// The release must describe exactly what was pinned: a mismatch means
	// it was replaced after the pin was reviewed.
	if rel.Commit != ref || rel.SHA256 != digest || rel.Repo != g.webRepo {
		return nil, fmt.Errorf("webbundle: %s doesn't match the pin in %s (commit %s, sha256 %s)",
			tag, g.pinRepo, rel.Short, rel.SHA256)
	}
	rel.SHA256 = digest
	if rel.tarballURL, ok = assets[rel.Tarball]; !ok {
		return nil, fmt.Errorf("webbundle: %s has no %s", tag, rel.Tarball)
	}
	return &rel, nil
}

// pinFile reads one line of pinRepo's file at pinRef.
func (g *GitHub) pinFile(ctx context.Context, path string) (string, error) {
	u := g.api + "/repos/" + g.pinRepo + "/contents/" + path + "?ref=" + url.QueryEscape(g.pinRef)
	var b strings.Builder
	if err := g.getAsset(ctx, u, 1<<10, func(r io.Reader) error {
		_, err := io.Copy(&b, r)
		return err
	}, "application/vnd.github.raw"); err != nil {
		return "", fmt.Errorf("webbundle: read the pin %s/%s: %w", g.pinRepo, path, err)
	}
	return strings.TrimSpace(b.String()), nil
}

// Tarball opens rel's tarball asset.
func (g *GitHub) Tarball(ctx context.Context, rel *Release) (io.ReadCloser, error) {
	if rel.tarballURL == "" {
		return nil, errors.New("webbundle: release has no tarball location")
	}
	resp, err := g.do(ctx, rel.tarballURL, "application/octet-stream")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (g *GitHub) getJSON(ctx context.Context, u string, v any) error {
	resp, err := g.do(ctx, u, "application/vnd.github+json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(v)
}

// getAsset reads at most limit bytes of u (as accept, default an asset's
// raw bytes) through read.
func (g *GitHub) getAsset(ctx context.Context, u string, limit int64, read func(io.Reader) error, accept ...string) error {
	a := "application/octet-stream"
	if len(accept) > 0 {
		a = accept[0]
	}
	resp, err := g.do(ctx, u, a)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return read(io.LimitReader(resp.Body, limit))
}

// do GETs u, which must be on the API host; the token never leaves it
// (dropAuthOffHost).
func (g *GitHub) do(ctx context.Context, u, accept string) (*http.Response, error) {
	if parsed, err := url.Parse(u); err != nil || !strings.HasPrefix(u, g.api+"/") || parsed.Host == "" {
		return nil, fmt.Errorf("webbundle: refusing to fetch %q: not on %s", u, g.api)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("webbundle: %w", err)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("webbundle: GitHub: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("webbundle: GitHub %s: %s", strings.TrimPrefix(u, g.api), resp.Status)
	}
	return resp, nil
}
