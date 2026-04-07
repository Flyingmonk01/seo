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
	"github.com/91astro/seo-agent/internal/services"

	openai "github.com/sashabaranov/go-openai"
)

type generateFAQPayload struct {
	MaxPages int `json:"max_pages"`
}

func (s *Server) handleGenerateFAQ(ctx context.Context, task *asynq.Task) error {
	var p generateFAQPayload
	if err := json.Unmarshal(task.Payload(), &p); err != nil || p.MaxPages == 0 {
		p.MaxPages = 10
	}

	log.Println("[faq] ─────────────────────────────────────────")
	log.Printf("[faq] Generating FAQ blocks for up to %d pages...", p.MaxPages)

	rawCol := s.db.Collection(models.ColRawData)

	// Find pages with question-intent queries (what, how, why, when, is, can, does)
	pipeline := []bson.D{
		{{Key: "$match", Value: bson.D{
			{Key: "query", Value: bson.D{
				{Key: "$regex", Value: "^(what|how|why|when|is |can |does |which)"},
				{Key: "$options", Value: "i"},
			}},
		}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$page"},
			{Key: "questions", Value: bson.D{{Key: "$addToSet", Value: "$query"}}},
			{Key: "totalImpressions", Value: bson.D{{Key: "$sum", Value: "$impressions"}}},
		}}},
		{{Key: "$match", Value: bson.D{
			{Key: "questions", Value: bson.D{{Key: "$exists", Value: true}}},
		}}},
		{{Key: "$addFields", Value: bson.D{
			{Key: "questionCount", Value: bson.D{{Key: "$size", Value: "$questions"}}},
		}}},
		{{Key: "$match", Value: bson.D{
			{Key: "questionCount", Value: bson.D{{Key: "$gte", Value: 3}}},
		}}},
		{{Key: "$sort", Value: bson.D{{Key: "totalImpressions", Value: -1}}}},
		{{Key: "$limit", Value: int64(p.MaxPages)}},
	}

	cursor, err := rawCol.Aggregate(ctx, pipeline)
	if err != nil {
		return fmt.Errorf("faq aggregate: %w", err)
	}
	defer cursor.Close(ctx)

	var results []struct {
		Page             string   `bson:"_id"`
		Questions        []string `bson:"questions"`
		TotalImpressions int64    `bson:"totalImpressions"`
	}
	if err := cursor.All(ctx, &results); err != nil {
		return err
	}

	log.Printf("[faq] Found %d pages with question-intent queries", len(results))

	generated := 0
	for _, r := range results {
		// Resolve to CMS page
		target, err := s.cms.ResolveTarget(r.Page)
		if err != nil {
			log.Printf("[faq]   ⊘ skip %s — %v", r.Page, err)
			continue
		}

		// Only add FAQ to pages collection (not posts — posts use Paragraph)
		if target.Collection != "pages" {
			log.Printf("[faq]   ⊘ skip %s — collection %q not supported for FAQ blocks", r.Page, target.Collection)
			continue
		}

		// Check if page already has FAQ block
		doc, err := s.cms.GetFullDocument("pages", target.DocID)
		if err != nil {
			log.Printf("[faq]   ⊘ skip %s — fetch failed: %v", r.Page, err)
			continue
		}
		layout, _ := doc["layout"].([]interface{})
		hasFAQ := false
		for _, block := range layout {
			if b, ok := block.(map[string]interface{}); ok {
				if b["blockType"] == "FAQ" {
					hasFAQ = true
					break
				}
			}
		}
		if hasFAQ {
			log.Printf("[faq]   ⊘ skip %s — already has FAQ block", r.Page)
			continue
		}

		// Take top 5 question queries
		questions := r.Questions
		if len(questions) > 5 {
			questions = questions[:5]
		}

		log.Printf("[faq]   → generating FAQ for %s (%d questions, %d impressions)",
			r.Page, len(questions), r.TotalImpressions)

		// GPT-4o generates FAQ answers
		faqs, err := s.generateFAQContent(ctx, r.Page, questions)
		if err != nil {
			log.Printf("[faq]   ✗ GPT-4o failed: %v", err)
			continue
		}

		// Add FAQ block to page via CMS
		if err := s.cms.AddFAQToPage(target.DocID, faqs); err != nil {
			log.Printf("[faq]   ✗ CMS update failed: %v", err)
			continue
		}

		log.Printf("[faq]   ✓ Added %d FAQs to %s", len(faqs), r.Page)

		// Record as a change for tracking
		suggCol := s.db.Collection(models.ColSuggestions)
		suggCol.InsertOne(ctx, models.SeoSuggestion{
			Page:        r.Page,
			Locale:      "en",
			PageSource:  models.PageSourceCMS,
			CMSPageID:   target.DocID,
			GeneratedBy: s.cfg.OpenAIModel,
			Status:      models.SuggestionLive,
			CreatedAt:   time.Now(),
		})

		generated++
	}

	log.Printf("[faq] ── Summary: added FAQ blocks to %d pages ──", generated)
	log.Println("[faq] ─────────────────────────────────────────")
	return nil
}

func (s *Server) generateFAQContent(ctx context.Context, page string, questions []string) ([]services.FAQBlockItem, error) {
	prompt := fmt.Sprintf(`You are an SEO content writer for 91Astrology, an Indian Vedic astrology platform.

Page: %s
Users are searching for these questions:
%s

Write a clear, helpful answer for each question. Output ONLY valid JSON:
[
  {"question": "exact question from above", "answer": "50-200 word answer"},
  ...
]

Rules:
- Answer each question directly and helpfully
- Use simple English accessible to Indian audience
- Include relevant Vedic astrology terminology where appropriate
- Each answer: 50-200 words
- Do NOT make up statistics or cite fake sources`,
		page,
		"- "+strings.Join(questions, "\n- "),
	)

	resp, err := s.openai.Client().CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: s.cfg.OpenAIModel,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: "You are an SEO content writer. Output only valid JSON, no markdown."},
			{Role: openai.ChatMessageRoleUser, Content: prompt},
		},
		Temperature: 0.7,
	})
	if err != nil {
		return nil, err
	}

	var items []services.FAQBlockItem
	if err := json.Unmarshal([]byte(resp.Choices[0].Message.Content), &items); err != nil {
		return nil, fmt.Errorf("parse FAQ response: %w", err)
	}

	// Filter out empty entries
	valid := items[:0]
	for _, item := range items {
		if item.Question != "" && item.Answer != "" {
			valid = append(valid, item)
		}
	}
	return valid, nil
}
