package workers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	openai "github.com/sashabaranov/go-openai"
	"github.com/91astro/seo-agent/internal/services"
)

type calendarBlogPayload struct {
	MaxPosts      int `json:"max_posts"`
	LookAheadDays int `json:"look_ahead_days"`
}

type upcomingEvent struct {
	Title    string `json:"title"`    // e.g. "Hanuman Jayanti 2026"
	Date     string `json:"date"`     // "YYYY-MM-DD"
	Query    string `json:"query"`    // SEO target query
	Category string `json:"category"` // Festival / Vedic / Zodiac
}

// fetchUpcomingEvents asks OpenAI for real upcoming Hindu/astrological events
// in the next lookAheadDays days. Returns events sorted by date ascending.
func (s *Server) fetchUpcomingEvents(ctx context.Context, from time.Time, lookAheadDays int) ([]upcomingEvent, error) {
	to := from.AddDate(0, 0, lookAheadDays)

	prompt := fmt.Sprintf(`Today is %s. List all significant Hindu festivals, Vedic astrological events, planetary transits, retrogrades, and eclipses happening between %s and %s (inclusive).

Use accurate dates based on the Hindu lunar calendar (tithi-based), Panchang, and astronomical data. Do NOT guess — only include events you are confident about.

For each event return:
- title: short event name with year (e.g. "Hanuman Jayanti 2026")
- date: exact date in YYYY-MM-DD format
- query: the best SEO search query to target for this event (English or Hinglish, 3-6 words, include year)
- category: one of "Festival", "Vedic", "Zodiac"

Output ONLY a valid JSON array, no explanation:
[
  {"title": "...", "date": "YYYY-MM-DD", "query": "...", "category": "..."},
  ...
]`,
		from.Format("2006-01-02"),
		from.Format("2006-01-02"),
		to.Format("2006-01-02"),
	)

	resp, err := s.openai.Client().CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: s.cfg.OpenAIModel,
		Messages: []openai.ChatCompletionMessage{
			{
				Role:    openai.ChatMessageRoleSystem,
				Content: "You are an expert in the Hindu Panchang, Vedic astrology calendar, and astronomical events. You know exact tithi-based dates for all Hindu festivals. Output only valid JSON arrays.",
			},
			{Role: openai.ChatMessageRoleUser, Content: prompt},
		},
		Temperature: 0.2, // low temp — we want factual dates not creative output
	})
	if err != nil {
		return nil, fmt.Errorf("fetch upcoming events: %w", err)
	}

	var events []upcomingEvent
	if err := json.Unmarshal([]byte(cleanLLMJSON(resp.Choices[0].Message.Content)), &events); err != nil {
		return nil, fmt.Errorf("parse events JSON: %w", err)
	}
	return events, nil
}

func (s *Server) handleCalendarBlogCreate(ctx context.Context, task *asynq.Task) error {
	var p calendarBlogPayload
	if err := json.Unmarshal(task.Payload(), &p); err != nil || p.MaxPosts == 0 {
		p.MaxPosts = 5
	}
	if p.LookAheadDays == 0 {
		p.LookAheadDays = 45
	}

	log.Println("[calendar-blog] ─────────────────────────────────────────")
	log.Printf("[calendar-blog] Fetching upcoming events for next %d days (max %d posts)...", p.LookAheadDays, p.MaxPosts)

	now := time.Now().UTC()

	events, err := s.fetchUpcomingEvents(ctx, now, p.LookAheadDays)
	if err != nil {
		return fmt.Errorf("calendar-blog: could not fetch events: %w", err)
	}
	log.Printf("[calendar-blog] OpenAI returned %d upcoming events", len(events))

	// Dedup against already-published cluster keys and headings
	usedClusterKeys := s.loadUsedClusterKeysFromCMS()
	existingPosts, err := s.cms.ListPosts(500, "en")
	if err != nil {
		log.Printf("[calendar-blog] WARN: could not list existing posts: %v", err)
	}
	existingHeadings := map[string]bool{}
	for _, post := range existingPosts {
		if h, ok := post["Heading"].(string); ok {
			existingHeadings[strings.ToLower(h)] = true
		}
	}

	created := 0
	for _, ev := range events {
		if created >= p.MaxPosts {
			break
		}

		eventDate, err := time.Parse("2006-01-02", ev.Date)
		if err != nil {
			log.Printf("[calendar-blog]   ! bad date %q for %q, skipping", ev.Date, ev.Title)
			continue
		}
		daysUntil := int(eventDate.Sub(now).Hours() / 24)

		clusterKey := services.ClusterKey(ev.Query)
		if usedClusterKeys[clusterKey] {
			log.Printf("[calendar-blog]   ~ skip %q — cluster key already used", ev.Title)
			continue
		}

		log.Printf("[calendar-blog] ── Event: %q on %s (%d days away) ──", ev.Title, ev.Date, daysUntil)

		customInstructions := fmt.Sprintf(
			"This article is for '%s' on %s (%d days away). "+
				"Include the exact date. Cover: what the event is, astrological significance, "+
				"Vedic remedies, puja vidhi, dos and don'ts, muhurat if applicable. "+
				"Make it timely, specific, and actionable for someone preparing for this event.",
			ev.Title, eventDate.Format("2 January 2006"), daysUntil,
		)

		post, err := s.generateBlogPostWithInstructions(ctx, ev.Query, 0, customInstructions)
		if err != nil {
			log.Printf("[calendar-blog]   x generation failed: %v", err)
			continue
		}

		if existingHeadings[strings.ToLower(post.Heading)] {
			log.Printf("[calendar-blog]   ~ skip — heading already exists: %q", post.Heading)
			continue
		}

		var imageID string
		if post.ImagePrompt != "" {
			imgID, err := s.generateAndUploadImage(ctx, post.ImagePrompt, post.Heading)
			if err != nil {
				log.Printf("[calendar-blog]   ! image generation failed (continuing): %v", err)
			} else {
				imageID = imgID
			}
		}

		categoryRelID := s.resolveCategoryID(ev.Category)
		authorID := s.resolveAuthorID()

		cmsPost := map[string]interface{}{
			"title":      post.Heading,
			"Heading":    post.Heading,
			"Date":       time.Now().Format("2 January 2006"),
			"Category":   ev.Category,
			"Content":    post.Content,
			"Paragraph":  post.Paragraphs,
			"Identifier": "en",
			"isHidden":   true,
			"meta": map[string]string{
				"title":       post.MetaTitle,
				"description": post.MetaDescription,
			},
		}
		if imageID != "" {
			cmsPost["image"] = imageID
		}
		if authorID != "" {
			cmsPost["author"] = authorID
		}
		if categoryRelID != "" {
			cmsPost["category"] = categoryRelID
		}

		docID, err := s.cms.CreatePost(cmsPost)
		if err != nil {
			log.Printf("[calendar-blog]   x CMS create failed: %v", err)
			continue
		}
		log.Printf("[calendar-blog]   + Created post %s: %q (hidden, needs review)", docID, post.Heading)

		topicID, err := s.cms.CreateTopic(map[string]interface{}{
			"query":      ev.Query,
			"clusterKey": clusterKey,
			"heading":    post.Heading,
			"post":       docID,
			"status":     "pending",
		})
		if err != nil {
			log.Printf("[calendar-blog]   ! WARN: could not save topic: %v", err)
		} else {
			log.Printf("[calendar-blog]   + Topic saved: %s", topicID)
		}

		usedClusterKeys[clusterKey] = true
		existingHeadings[strings.ToLower(post.Heading)] = true
		created++
	}

	log.Printf("[calendar-blog] ── Summary: created %d event-based posts (hidden) ──", created)
	log.Println("[calendar-blog] ─────────────────────────────────────────")
	return nil
}
