package workers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"time"

	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
	"github.com/91astro/seo-agent/internal/agent"
	"github.com/91astro/seo-agent/internal/models"
)

type generateContentPayload struct {
	TopN int `json:"top_n"`
}

func (s *Server) handleGenerateContent(ctx context.Context, task *asynq.Task) error {
	var p generateContentPayload
	if err := json.Unmarshal(task.Payload(), &p); err != nil || p.TopN == 0 {
		p.TopN = 20
	}

	log.Println("[content] ─────────────────────────────────────────")
	log.Printf("[content] Generating content for top %d issues (sorted by priority score)...", p.TopN)

	issueCol := s.db.Collection(models.ColIssues)
	suggCol := s.db.Collection(models.ColSuggestions)

	cursor, err := issueCol.Find(ctx,
		bson.D{{Key: "status", Value: models.IssuePendingGeneration}},
		options.Find().SetSort(bson.D{{Key: "priority_score", Value: -1}}).SetLimit(int64(p.TopN)),
	)
	if err != nil {
		return err
	}
	defer cursor.Close(ctx)

	var issues []models.SeoIssue
	if err := cursor.All(ctx, &issues); err != nil {
		return err
	}

	log.Printf("[content] Found %d issues pending generation", len(issues))

	generated := 0
	for i, issue := range issues {
		log.Printf("[content] ── Issue %d/%d ──────────────────────────", i+1, len(issues))
		log.Printf("[content]   Page:        %s", issue.Page)
		log.Printf("[content]   Issue type:  %s", issue.IssueType)
		log.Printf("[content]   Top query:   %q", issue.TopQuery)
		log.Printf("[content]   Impressions: %d | CTR: %.2f%% | Position: %.1f | Score: %.1f",
			issue.Metrics.TotalImpressions,
			issue.Metrics.AvgCTR,
			issue.Metrics.AvgPosition,
			issue.PriorityScore,
		)

		// ── Detect page source ─────────────────────────────────────────────────
		pageSource, cmsPageID, current := s.detectPageSource(issue.Page)
		if pageSource == "skip" {
			log.Printf("[content]   ⊘ skipping — page not manageable via CMS")
			continue
		}
		log.Printf("[content]   Page source: %q | CMS ID: %q", pageSource, cmsPageID)
		log.Printf("[content]   Current title: %q", current.Title)
		log.Printf("[content]   Current meta:  %q", current.MetaDescription)

		// ── Route through SEOAgent (structured I/O, detailed logging) ────────
		log.Printf("[content]   → dispatching to seo_agent...")
		agentOut, err := s.seoAgent.Execute(ctx, agent.AgentInput{
			Task: "generate_seo_content",
			Payload: &agent.SEOContentInput{
				Issue:      &issue,
				Current:    current,
				PageSource: pageSource,
				CMSPageID:  cmsPageID,
			},
		})
		if err != nil {
			log.Printf("[content]   ✗ seo_agent failed: %v", err)
			continue
		}

		seoOut, ok := agentOut.Data.(agent.SEOContentOutput)
		if !ok {
			log.Printf("[content]   ✗ seo_agent returned unexpected type: %T", agentOut.Data)
			continue
		}
		if err := validateSEOProposal(seoOut.Proposal); err != nil {
			log.Printf("[content]   ✗ proposal validation failed: %v", err)
			continue
		}

		proposal := seoOut.Proposal
		suggestion := models.SeoSuggestion{
			IssueID:     issue.ID,
			Page:        issue.Page,
			Locale:      proposal.Locale,
			PageSource:  proposal.PageSource,
			CMSPageID:   proposal.CMSPageID,
			Current:     proposal.Current,
			Proposed:    proposal.Proposed,
			GeneratedBy: s.cfg.OpenAIModel,
			Status:      models.SuggestionPending,
			CreatedAt:   time.Now(),
		}

		if _, err := suggCol.InsertOne(ctx, suggestion); err != nil {
			log.Printf("[content]   ✗ Insert suggestion failed: %v", err)
			continue
		}

		// Mirror to CMS so the dashboard can read/approve without hitting the Go server.
		cmsSuggID, cmsErr := s.cms.CreateSuggestion(map[string]interface{}{
			"page":                    suggestion.Page,
			"locale":                  suggestion.Locale,
			"cmsPageId":               suggestion.CMSPageID,
			"cmsCollection":           suggestion.PageSource,
			"currentTitle":            suggestion.Current.Title,
			"currentMetaDescription":  suggestion.Current.MetaDescription,
			"proposedTitle":           suggestion.Proposed.Title,
			"proposedMetaDescription": suggestion.Proposed.MetaDescription,
			"generatedBy":             suggestion.GeneratedBy,
			"status":                  "pending",
		})
		if cmsErr != nil {
			log.Printf("[content]   ! CMS suggestion mirror failed (non-fatal): %v", cmsErr)
		} else {
			log.Printf("[content]   ✓ CMS suggestion created: %s", cmsSuggID)
		}

		// Mark issue as pending approval
		issueCol.UpdateByID(ctx, issue.ID, bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: models.IssuePendingApproval},
		}}})

		log.Printf("[content]   ✓ Suggestion saved — waiting for human approval in dashboard")
		generated++
	}

	log.Println("[content] ── Summary ────────────────────────────────")
	log.Printf("[content]   Generated: %d suggestions", generated)
	log.Printf("[content]   Failed:    %d", len(issues)-generated)
	log.Println("[content] ─────────────────────────────────────────")
	return nil
}

// detectPageSource probes Payload CMS to determine where a page's metadata lives.
// Returns (pageSource, cmsPageID, currentContent).
// Returns "skip" as pageSource for pages that cannot be managed via CMS (celebrity, astrologer).
func (s *Server) detectPageSource(pageURL string) (string, string, models.SEOContent) {
	empty := models.SEOContent{Title: pageURL}

	// Skip pages we can't update — celebrity/astrologer pages have UUIDs in the URL
	// and are managed by 91astro-api which is out of scope.
	uuidPattern := regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-`)
	if uuidPattern.MatchString(pageURL) {
		return "skip", "", empty
	}

	if s.cms == nil || !s.cms.IsConfigured() {
		return "skip", "", empty
	}

	target, err := s.cms.ResolveTarget(pageURL)
	if err != nil {
		log.Printf("[content]   WARN: CMS resolve failed: %v — skipping", err)
		return "skip", "", empty
	}

	// Fetch the actual current meta from CMS using the resolved document ID
	current := empty
	if doc, fetchErr := s.cms.GetDocumentByID(target.Collection, target.DocID); fetchErr == nil && doc != nil {
		current = models.SEOContent{
			Title:           doc.Meta.Title,
			MetaDescription: doc.Meta.Description,
		}
		if doc.Heading != "" && current.Title == "" {
			current.Title = doc.Heading
		}
	}

	return models.PageSourceCMS, target.DocID, current
}

// validateSEOProposal enforces minimum quality constraints on a proposal
// before it is persisted.
func validateSEOProposal(p agent.SEOChangeProposal) error {
	if p.Proposed.Title == "" {
		return fmt.Errorf("proposed title is empty")
	}
	if len(p.Proposed.Title) > 60 {
		return fmt.Errorf("proposed title too long: %d chars (max 60)", len(p.Proposed.Title))
	}
	if p.Proposed.MetaDescription == "" {
		return fmt.Errorf("proposed meta description is empty")
	}
	if len(p.Proposed.MetaDescription) > 160 {
		return fmt.Errorf("proposed meta description too long: %d chars (max 160)", len(p.Proposed.MetaDescription))
	}
	return nil
}
