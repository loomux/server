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

// GitHub lists a repository's web-<sha> pre-releases through the GitHub
// API with a read-only token.
//
// What makes a release genuine: loomux/server pins the bundle its image
// is built with by sha256 in its own repo (deploy/web-sha256), but an
// in-place update has no such reviewed pin to check against, so it trusts
// what loomux/web's CI publishes and nothing else — a release created by
// github-actions[bot] (not a person's upload), whose tag, commit and
// tarball agree, and whose commit is on the repo's main branch. The
// tarball is then checked against the release's sha256 by the Manager.
type GitHub struct {
	api    string
	repo   string
	token  string
	client *http.Client
}

// ciAuthor is the login of releases created by GitHub Actions.
const ciAuthor = "github-actions[bot]"

// NewGitHub is a Source for repo ("owner/name") authenticating with token.
func NewGitHub(repo, token string) *GitHub {
	return &GitHub{api: "https://api.github.com", repo: repo, token: token,
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

type ghRelease struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Author     struct {
		Login string `json:"login"`
	} `json:"author"`
	Assets []struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"assets"`
}

// Latest is the newest genuine web-<sha> release. Releases come newest
// first; one that doesn't check out is skipped, not trusted.
func (g *GitHub) Latest(ctx context.Context) (*Release, error) {
	var list []ghRelease
	if err := g.getJSON(ctx, g.api+"/repos/"+g.repo+"/releases?per_page=20", &list); err != nil {
		return nil, err
	}
	var skipped []string
	for _, gr := range list {
		if gr.Draft || !strings.HasPrefix(gr.TagName, "web-") {
			continue
		}
		rel, err := g.check(ctx, gr)
		if err != nil {
			skipped = append(skipped, err.Error())
			continue
		}
		return rel, nil
	}
	if len(skipped) > 0 {
		return nil, fmt.Errorf("webbundle: no genuine web release in %s: %s", g.repo, strings.Join(skipped, "; "))
	}
	return nil, fmt.Errorf("webbundle: %s has no web releases", g.repo)
}

func (g *GitHub) check(ctx context.Context, gr ghRelease) (*Release, error) {
	if gr.Author.Login != ciAuthor {
		return nil, fmt.Errorf("%s was published by %q, not CI", gr.TagName, gr.Author.Login)
	}
	assets := map[string]string{}
	for _, a := range gr.Assets {
		assets[a.Name] = a.URL
	}
	jsonURL, ok := assets[releaseFile]
	if !ok {
		return nil, fmt.Errorf("%s has no %s", gr.TagName, releaseFile)
	}
	var rel Release
	if err := g.getAsset(ctx, jsonURL, 1<<16, func(r io.Reader) error { return json.NewDecoder(r).Decode(&rel) }); err != nil {
		return nil, fmt.Errorf("%s: %w", gr.TagName, err)
	}
	if err := rel.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", gr.TagName, err)
	}
	if rel.Tag != gr.TagName || rel.Repo != g.repo {
		return nil, fmt.Errorf("%s: its %s names %s in %s", gr.TagName, releaseFile, rel.Tag, rel.Repo)
	}
	rel.tarballURL, ok = assets[rel.Tarball]
	if !ok {
		return nil, fmt.Errorf("%s has no %s", gr.TagName, rel.Tarball)
	}
	var cmp struct {
		Status string `json:"status"`
	}
	if err := g.getJSON(ctx, g.api+"/repos/"+g.repo+"/compare/"+rel.Commit+"...main", &cmp); err != nil {
		return nil, fmt.Errorf("%s: %w", gr.TagName, err)
	}
	if cmp.Status != "ahead" && cmp.Status != "identical" {
		return nil, fmt.Errorf("%s: commit %s is not on main", gr.TagName, rel.Short)
	}
	return &rel, nil
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

func (g *GitHub) getAsset(ctx context.Context, u string, limit int64, read func(io.Reader) error) error {
	resp, err := g.do(ctx, u, "application/octet-stream")
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
