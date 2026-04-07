package workers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/bson"
	"github.com/91astro/seo-agent/internal/models"

	openai "github.com/sashabaranov/go-openai"
)

type refreshContentPayload struct {
	MaxPosts  int `json:"max_posts"`
	StaleDays int `json:"stale_days"`
}

func (s *Server) handleRefreshContent(ctx context.Context, task *asynq.Task) error {
	var p refreshContentPayload
	if err := json.Unmarshal(task.Payload(), &p); err != nil {
		p.MaxPosts = 5
		p.StaleDays = 180
	}
	if p.MaxPosts == 0 {
		p.MaxPosts = 5
	}
	if p.StaleDays == 0 {
		p.StaleDays = 180
	}

	log.Println("[refresher] ─────────────────────────────────────────")
	log.Printf("[refresher] Refreshing up to %d stale blog posts (>%d days old, declining CTR)...", p.MaxPosts, p.StaleDays)

	// Find pages with declining CTR: compare recent 30 days vs previous 30 days
	rawCol := s.db.Collection(models.ColRawData)

	now := time.Now()
	recent := now.AddDate(0, 0, -30).Format("2006-01-02")
	older := now.AddDate(0, 0, -60).Format("2006-01-02")

	// Get recent CTR per page
	recentPipeline := []bson.D{
		{{Key: "$match", Value: bson.D{{Key: "date", Value: bson.D{{Key: "$gte", Value: recent}}}}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$page"},
			{Key: "avgCTR", Value: bson.D{{Key: "$avg", Value: "$ctr"}}},
			{Key: "totalImpressions", Value: bson.D{{Key: "$sum", Value: "$impressions"}}},
			{Key: "topQuery", Value: bson.D{{Key: "$first", Value: "$query"}}},
		}}},
		{{Key: "$match", Value: bson.D{
			{Key: "totalImpressions", Value: bson.D{{Key: "$gte", Value: 50}}},
		}}},
	}

	cursor, err := rawCol.Aggregate(ctx, recentPipeline)
	if err != nil {
		return fmt.Errorf("refresh recent aggregate: %w", err)
	}
	var recentData []struct {
		Page             string  `bson:"_id"`
		AvgCTR           float64 `bson:"avgCTR"`
		TotalImpressions int64   `bson:"totalImpressions"`
		TopQuery         string  `bson:"topQuery"`
	}
	cursor.All(ctx, &recentData)
	cursor.Close(ctx)

	// Get older CTR per page
	olderPipeline := []bson.D{
		{{Key: "$match", Value: bson.D{
			{Key: "date", Value: bson.D{
				{Key: "$gte", Value: older},
				{Key: "$lt", Value: recent},
			}},
		}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$page"},
			{Key: "avgCTR", Value: bson.D{{Key: "$avg", Value: "$ctr"}}},
		}}},
	}

	cursor2, err := rawCol.Aggregate(ctx, olderPipeline)
	if err != nil {
		return fmt.Errorf("refresh older aggregate: %w", err)
	}
	olderMap := map[string]float64{}
	var olderData []struct {
		Page   string  `bson:"_id"`
		AvgCTR float64 `bson:"avgCTR"`
	}
	cursor2.All(ctx, &olderData)
	cursor2.Close(ctx)
	for _, o := range olderData {
		olderMap[o.Page] = o.AvgCTR
	}

	// Find pages with declining CTR (blog posts only)
	type declineCandidate struct {
		page     string
		ctrDelta float64
		topQuery string
		impr     int64
	}
	var candidates []declineCandidate
	for _, r := range recentData {
		olderCTR, exists := olderMap[r.Page]
		if !exists {
			continue
		}
		delta := r.AvgCTR - olderCTR
		if delta < -0.5 { // CTR dropped by more than 0.5 percentage points
			// Only blog posts (URL contains /blogs/)
			if !isaBlogURL(r.Page) {
				continue
			}
			candidates = append(candidates, declineCandidate{
				page: r.Page, ctrDelta: delta, topQuery: r.TopQuery, impr: r.TotalImpressions,
			})
		}
	}

	log.Printf("[refresher] Found %d blog posts with declining CTR", len(candidates))

	refreshed := 0
	for _, c := range candidates {
		if refreshed >= p.MaxPosts {
			break
		}

		log.Printf("[refresher]   → %s (CTR delta: %.2f%%, query: %q, impr: %d)",
			c.page, c.ctrDelta, c.topQuery, c.impr)

		// Resolve CMS target
		target, err := s.cms.ResolveTarget(c.page)
		if err != nil {
			log.Printf("[refresher]   ⊘ skip — %v", err)
			continue
		}

		// Fetch current post content
		doc, err := s.cms.GetFullDocument(target.Collection, target.DocID)
		if err != nil {
			log.Printf("[refresher]   ⊘ skip — fetch failed: %v", err)
			continue
		}

		currentHeading, _ := doc["Heading"].(string)

		// GPT-4o generates improved meta
		improved, err := s.generateRefreshedMeta(ctx, c.page, c.topQuery, currentHeading)
		if err != nil {
			log.Printf("[refresher]   ✗ GPT-4o failed: %v", err)
			continue
		}

		// Update meta via CMS
		switch target.Collection {
		case "Posts":
			err = s.cms.UpdatePostMeta(target.DocID, improved.Title, improved.Description)
		default:
			err = s.cms.UpdatePageMeta(target.DocID, improved.Title, improved.Description)
		}
		if err != nil {
			log.Printf("[refresher]   ✗ CMS update failed: %v", err)
			continue
		}

		log.Printf("[refresher]   ✓ Refreshed: %q → %q", currentHeading, improved.Title)

		// Record change for tracking
		changeCol := s.db.Collection(models.ColChanges)
		changeCol.InsertOne(ctx, models.SeoChange{
			Page:      c.page,
			AppliedAt: time.Now(),
			RollbackData: models.SEOContent{
				Title:           currentHeading,
				MetaDescription: "", // we don't have old meta desc easily
			},
		})

		refreshed++
	}

	log.Printf("[refresher] ── Summary: refreshed %d blog posts ──", refreshed)
	log.Println("[refresher] ─────────────────────────────────────────")
	return nil
}

type refreshedMeta struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}

func (s *Server) generateRefreshedMeta(ctx context.Context, page, topQuery, currentHeading string) (*refreshedMeta, error) {
	prompt := fmt.Sprintf(`You are an SEO specialist for 91Astrology.

This blog post has declining CTR and needs a refreshed title and description.

Page: %s
Top search query: "%s"
Current heading: "%s"

Generate improved metadata. Output ONLY valid JSON:
{
  "title": "improved heading/title (max 60 chars, include keyword naturally)",
  "description": "improved meta description (max 155 chars, include keyword + CTA)"
}

Rules:
- Include the top query keyword naturally
- Make the title more compelling and click-worthy
- Add urgency or specificity (year, numbers, actionable)
- Do NOT use clickbait or fake superlatives
- Target Indian audience`,
		page, topQuery, currentHeading,
	)

	resp, err := s.openai.Client().CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: s.cfg.OpenAIModel,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: "You are an SEO specialist. Output only valid JSON."},
			{Role: openai.ChatMessageRoleUser, Content: prompt},
		},
		Temperature: 0.5,
	})
	if err != nil {
		return nil, err
	}

	var meta refreshedMeta
	if err := json.Unmarshal([]byte(resp.Choices[0].Message.Content), &meta); err != nil {
		return nil, fmt.Errorf("parse refreshed meta: %w", err)
	}
	return &meta, nil
}

func isaBlogURL(u string) bool {
	return strings.Contains(u, "/blogs/") || strings.Contains(u, "/blog/")
}
