package agent

import "github.com/91astro/seo-agent/internal/models"

// ── SEO Agent I/O ─────────────────────────────────────────────────────────────

// SEOContentInput is the typed payload for the SEOAgent "generate_seo_content" task.
type SEOContentInput struct {
	Issue      *models.SeoIssue
	Current    models.SEOContent
	PageSource models.PageSource
	CMSPageID  string
}

// SEOContentOutput is the typed result from SEOAgent.Execute.
type SEOContentOutput struct {
	Proposal SEOChangeProposal
}

// SEOChangeProposal is the canonical structured output for any AI-driven
// SEO metadata change. ALL AI decisions that affect page metadata must
// produce this struct — no free-text, no map[string]interface{}.
//
// Fields intentionally mirror models.SeoSuggestion so they can be
// persisted without transformation.
type SEOChangeProposal struct {
	// Identifying context
	Page       string            `json:"page"`
	Locale     string            `json:"locale"`
	PageSource models.PageSource `json:"pageSource"`
	CMSPageID  string            `json:"cmsPageId,omitempty"`
	IssueType  string            `json:"issueType"`

	// Priority from the analyst stage
	PriorityScore float64 `json:"priorityScore"`

	// Content delta
	Current  models.SEOContent `json:"current"`
	Proposed models.SEOContent `json:"proposed"`

	// Quality signals
	Confidence float64 `json:"confidence"` // 0.0–1.0; 0 = not yet evaluated
	Reasoning  string  `json:"reasoning"`  // why the change is expected to improve rank
}

