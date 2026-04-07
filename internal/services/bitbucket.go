package services

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/plumbing/object"
)

type PRResult struct {
	URL    string
	Number int
}

type BitbucketService struct {
	token      string // Bitbucket API token (replaces app passwords as of Sep 2025)
	workspace  string // Bitbucket workspace slug
	httpClient *http.Client
}

func NewBitbucketService(token, workspace string) *BitbucketService {
	return &BitbucketService{
		token:     token,
		workspace: workspace,
		httpClient:  &http.Client{Timeout: 30 * time.Second},
	}
}

// CreateFeaturePR clones a repo, writes files to a new branch, and opens a PR.
func (b *BitbucketService) CreateFeaturePR(repoSlug, branchName, prTitle, prBody string, files map[string]string) (*PRResult, error) {
	tmpDir, err := os.MkdirTemp("", "seo-agent-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	cloneURL := fmt.Sprintf("https://bitbucket.org/%s/%s.git", b.workspace, repoSlug)
	// API tokens use x-token-auth as username for git operations
	auth := &githttp.BasicAuth{Username: "x-token-auth", Password: b.token}

	r, err := git.PlainClone(tmpDir, false, &git.CloneOptions{
		URL:      cloneURL,
		Auth:     auth,
		Depth:    1,
	})
	if err != nil {
		return nil, fmt.Errorf("clone %s: %w", repoSlug, err)
	}

	w, err := r.Worktree()
	if err != nil {
		return nil, err
	}

	// Create and checkout new branch
	if err := w.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName(branchName),
		Create: true,
	}); err != nil {
		return nil, fmt.Errorf("checkout branch: %w", err)
	}

	// Write generated files
	for filePath, content := range files {
		fullPath := filepath.Join(tmpDir, filePath)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
			return nil, err
		}
		if _, err := w.Add(filePath); err != nil {
			return nil, fmt.Errorf("git add %s: %w", filePath, err)
		}
	}

	// Commit
	if _, err := w.Commit(fmt.Sprintf("feat(seo-agent): %s", prTitle), &git.CommitOptions{
		Author: &object.Signature{
			Name:  "91Astro SEO Agent",
			Email: "seo-agent@91astrology.com",
			When:  time.Now(),
		},
	}); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	// Push to Bitbucket
	if err := r.Push(&git.PushOptions{
		Auth: auth,
	}); err != nil {
		return nil, fmt.Errorf("push: %w", err)
	}

	// Open PR via Bitbucket API
	return b.createPR(repoSlug, branchName, prTitle, prBody)
}

func (b *BitbucketService) createPR(repoSlug, branchName, title, description string) (*PRResult, error) {
	payload := map[string]interface{}{
		"title":       fmt.Sprintf("[SEO Agent] %s", title),
		"description": description,
		"source": map[string]interface{}{
			"branch": map[string]string{"name": branchName},
		},
		"destination": map[string]interface{}{
			"branch": map[string]string{"name": "master"},
		},
		"close_source_branch": true,
	}

	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("https://api.bitbucket.org/2.0/repositories/%s/%s/pullrequests", b.workspace, repoSlug)

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// API tokens use Bearer auth for REST API calls
	req.Header.Set("Authorization", "Bearer "+b.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("create PR: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		var errBody map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&errBody)
		return nil, fmt.Errorf("Bitbucket API %d: %v", resp.StatusCode, errBody)
	}

	var pr struct {
		ID    int    `json:"id"`
		Links struct {
			HTML struct{ Href string } `json:"html"`
		} `json:"links"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, err
	}

	return &PRResult{URL: pr.Links.HTML.Href, Number: pr.ID}, nil
}

// GetFileContent fetches the raw content of a file from the default branch.
// Returns ("", nil) when the file does not exist yet (new file).
func (b *BitbucketService) GetFileContent(repoSlug, filePath string) (string, error) {
	url := fmt.Sprintf("https://api.bitbucket.org/2.0/repositories/%s/%s/src/HEAD/%s",
		b.workspace, repoSlug, filePath)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+b.token)

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch file %s: %w", filePath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return "", nil // new file
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("fetch file returned %d", resp.StatusCode)
	}

	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	return buf.String(), nil
}

// BuildBBPRBody formats a readable PR description for Bitbucket.
func BuildBBPRBody(hypothesis string, signals []string, changes []string, flagKey string) string {
	var sb strings.Builder
	sb.WriteString("## What & Why\n")
	sb.WriteString(hypothesis + "\n\n")
	sb.WriteString("## Research Signals\n")
	for _, s := range signals {
		sb.WriteString("- " + s + "\n")
	}
	sb.WriteString("\n## Changes\n")
	for _, c := range changes {
		sb.WriteString("- " + c + "\n")
	}
	sb.WriteString(fmt.Sprintf("\n## Feature Flag\n`%s` — starts at 5%% rollout after merge.\n\n", flagKey))
	sb.WriteString("---\n*Generated by 91Astro SEO Agent. Requires human review before merge.*\n")
	return sb.String()
}
