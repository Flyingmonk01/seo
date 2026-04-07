package agent

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/91astro/seo-agent/internal/services"
)

// SEOAgent is the agent responsible for generating structured SEO metadata
// change proposals. It wraps the existing OpenAIService without replacing it.
//
// Responsibility boundary:
//   - Accepts a typed SEOContentInput
//   - Delegates to services.OpenAIService.GenerateSEOContent (unchanged)
//   - Returns a structured SEOChangeProposal (never raw AI text)
//
// NOT responsible for: CMS resolution, DB writes, approval flow.
// Those remain in the worker layer for now.
type SEOAgent struct {
	openai *services.OpenAIService
}

// NewSEOAgent constructs an SEOAgent.
// The openai parameter is the same instance already held by workers.Server,
// so there is no second client or token overhead.
func NewSEOAgent(openai *services.OpenAIService) *SEOAgent {
	return &SEOAgent{openai: openai}
}

// Name returns the stable agent identifier used in logs and (future) memory records.
func (a *SEOAgent) Name() string { return "seo_agent" }

// Execute generates a structured SEO change proposal for a single page issue.
//
// Expected input:  AgentInput{Task: "generate_seo_content", Payload: *SEOContentInput}
// Returns:         AgentOutput{Data: SEOContentOutput}
//
// All steps are logged with [seo_agent] prefix so they are easy to grep.
func (a *SEOAgent) Execute(ctx context.Context, input AgentInput) (AgentOutput, error) {
	start := time.Now()

	log.Printf("[seo_agent] ── Execute start ─────────────────────────")
	log.Printf("[seo_agent]   task=%q", input.Task)

	// ── 1. Type-assert input ──────────────────────────────────────────────────
	in, ok := input.Payload.(*SEOContentInput)
	if !ok {
		err := fmt.Errorf("seo_agent: expected *SEOContentInput, got %T", input.Payload)
		log.Printf("[seo_agent] ✗ input type error: %v", err)
		return AgentOutput{AgentName: a.Name(), Success: false, Error: err.Error()}, err
	}

	log.Printf("[seo_agent]   page=%q", in.Issue.Page)
	log.Printf("[seo_agent]   issue_type=%q  priority=%.1f", in.Issue.IssueType, in.Issue.PriorityScore)
	log.Printf("[seo_agent]   top_query=%q", in.Issue.TopQuery)
	log.Printf("[seo_agent]   impressions=%d  ctr=%.2f%%  position=%.1f",
		in.Issue.Metrics.TotalImpressions,
		in.Issue.Metrics.AvgCTR,
		in.Issue.Metrics.AvgPosition,
	)
	log.Printf("[seo_agent]   page_source=%q  cms_page_id=%q", in.PageSource, in.CMSPageID)
	log.Printf("[seo_agent]   current_title=%q", in.Current.Title)
	log.Printf("[seo_agent]   current_meta=%q", in.Current.MetaDescription)

	// ── 2. Delegate to existing OpenAI service (no logic change) ─────────────
	log.Printf("[seo_agent]   → calling OpenAI.GenerateSEOContent...")

	result, err := a.openai.GenerateSEOContent(ctx, in.Issue, in.Current)
	if err != nil {
		log.Printf("[seo_agent] ✗ OpenAI call failed: %v", err)
		return AgentOutput{
			AgentName: a.Name(),
			Success:   false,
			Error:     err.Error(),
		}, fmt.Errorf("seo_agent: content generation: %w", err)
	}

	// ── 3. Build structured proposal (no free text leaks out) ─────────────────
	proposal := SEOChangeProposal{
		Page:          in.Issue.Page,
		Locale:        "en", // default; locale routing lives in the worker layer
		PageSource:    in.PageSource,
		CMSPageID:     in.CMSPageID,
		IssueType:     string(in.Issue.IssueType),
		PriorityScore: in.Issue.PriorityScore,
		Current:       result.Current,
		Proposed:      result.Proposed,
		Confidence:    0,
		Reasoning:     "",
	}

	// ── 4. Log output detail ──────────────────────────────────────────────────
	log.Printf("[seo_agent] ✓ proposal ready  elapsed=%s", time.Since(start).Round(time.Millisecond))
	log.Printf("[seo_agent]   proposed_title=%q", proposal.Proposed.Title)
	log.Printf("[seo_agent]   proposed_meta=%q", proposal.Proposed.MetaDescription)

	if len(proposal.Proposed.FAQSchema) > 0 {
		log.Printf("[seo_agent]   faq_count=%d", len(proposal.Proposed.FAQSchema))
		for i, faq := range proposal.Proposed.FAQSchema {
			log.Printf("[seo_agent]   faq[%d]: q=%q", i+1, faq.Question)
		}
	}

	log.Printf("[seo_agent] ── Execute end ───────────────────────────")

	return AgentOutput{
		AgentName: a.Name(),
		Success:   true,
		Data:      SEOContentOutput{Proposal: proposal},
	}, nil
}
