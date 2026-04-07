package workers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/91astro/seo-agent/internal/models"
	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/bson"

	openai "github.com/sashabaranov/go-openai"
)

type createContentPayload struct {
	MaxPosts int `json:"max_posts"`
}

func (s *Server) handleCreateContent(ctx context.Context, task *asynq.Task) error {
	var p createContentPayload
	if err := json.Unmarshal(task.Payload(), &p); err != nil || p.MaxPosts == 0 {
		p.MaxPosts = 3
	}

	log.Println("[content-creator] ─────────────────────────────────────────")
	log.Printf("[content-creator] Creating up to %d new blog posts for content gaps...", p.MaxPosts)

	rawCol := s.db.Collection(models.ColRawData)

	// Find high-impression queries that don't match any existing page on the site.
	// These are content gaps — queries people search for but we have no page for.
	pipeline := []bson.D{
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$query"},
			{Key: "totalImpressions", Value: bson.D{{Key: "$sum", Value: "$impressions"}}},
			{Key: "totalClicks", Value: bson.D{{Key: "$sum", Value: "$clicks"}}},
			{Key: "avgPosition", Value: bson.D{{Key: "$avg", Value: "$position"}}},
			{Key: "pages", Value: bson.D{{Key: "$addToSet", Value: "$page"}}},
		}}},
		{{Key: "$match", Value: bson.D{
			{Key: "totalImpressions", Value: bson.D{{Key: "$gte", Value: 100}}},
			{Key: "avgPosition", Value: bson.D{{Key: "$gte", Value: 10}}}, // we rank poorly
		}}},
		{{Key: "$sort", Value: bson.D{{Key: "totalImpressions", Value: -1}}}},
		{{Key: "$limit", Value: int64(p.MaxPosts * 3)}}, // fetch extra to filter
	}

	cursor, err := rawCol.Aggregate(ctx, pipeline)
	if err != nil {
		return fmt.Errorf("content gap aggregate: %w", err)
	}
	defer cursor.Close(ctx)

	var gaps []struct {
		Query            string   `bson:"_id"`
		TotalImpressions int64    `bson:"totalImpressions"`
		TotalClicks      int64    `bson:"totalClicks"`
		AvgPosition      float64  `bson:"avgPosition"`
		Pages            []string `bson:"pages"`
	}
	if err := cursor.All(ctx, &gaps); err != nil {
		return err
	}

	// Fetch existing posts to avoid duplicates
	existingPosts, err := s.cms.ListPosts(500, "en")
	if err != nil {
		log.Printf("[content-creator] WARN: could not list existing posts: %v", err)
	}
	existingHeadings := map[string]bool{}
	for _, post := range existingPosts {
		if h, ok := post["Heading"].(string); ok {
			existingHeadings[strings.ToLower(h)] = true
		}
	}

	log.Printf("[content-creator] Found %d potential content gaps", len(gaps))

	created := 0
	for _, gap := range gaps {
		if created >= p.MaxPosts {
			break
		}

		// Skip if query is too short or generic
		if len(gap.Query) < 10 {
			continue
		}

		log.Printf("[content-creator] ── Gap: %q (%d impressions, pos %.1f) ──",
			gap.Query, gap.TotalImpressions, gap.AvgPosition)

		// Generate blog post via GPT-4o
		post, err := s.generateBlogPost(ctx, gap.Query, gap.TotalImpressions)
		if err != nil {
			log.Printf("[content-creator]   ✗ generation failed: %v", err)
			continue
		}

		// Check for duplicate heading
		if existingHeadings[strings.ToLower(post.Heading)] {
			log.Printf("[content-creator]   ⊘ skip — heading already exists: %q", post.Heading)
			continue
		}

		// Get a default author and category from CMS
		authorID, categoryID := s.getDefaultAuthorAndCategory(ctx)

		// Create post in CMS
		cmsPost := map[string]interface{}{
			"title":      post.Heading,
			"Heading":    post.Heading,
			"Date":       time.Now().Format("2 January 2006"),
			"Category":   post.Category,
			"Content":    post.Content,
			"Paragraph":  post.Paragraphs,
			"Identifier": "en",
			"isHidden":   true, // start hidden — human reviews before publishing
			"meta": map[string]string{
				"title":       post.MetaTitle,
				"description": post.MetaDescription,
			},
		}
		if authorID != "" {
			cmsPost["author"] = authorID
		}
		if categoryID != "" {
			cmsPost["category"] = categoryID
		}

		docID, err := s.cms.CreatePost(cmsPost)
		if err != nil {
			log.Printf("[content-creator]   ✗ CMS create failed: %v", err)
			continue
		}

		log.Printf("[content-creator]   ✓ Created post %s: %q (hidden, needs review)", docID, post.Heading)

		// Record for tracking
		s.db.Collection(models.ColSuggestions).InsertOne(ctx, models.SeoSuggestion{
			Page:       fmt.Sprintf("new-post:%s", docID),
			Locale:     "en",
			PageSource: models.PageSourceCMS,
			CMSPageID:  docID,
			Proposed: models.SEOContent{
				Title:           post.MetaTitle,
				MetaDescription: post.MetaDescription,
			},
			GeneratedBy: s.cfg.OpenAIModel,
			Status:      models.SuggestionPending,
			CreatedAt:   time.Now(),
		})

		created++
	}

	log.Printf("[content-creator] ── Summary: created %d new blog posts (hidden) ──", created)
	log.Println("[content-creator] ─────────────────────────────────────────")
	return nil
}

type generatedPost struct {
	Heading         string        `json:"heading"`
	MetaTitle       string        `json:"metaTitle"`
	MetaDescription string        `json:"metaDescription"`
	Category        string        `json:"category"`
	Content         []interface{} `json:"content"`
	Paragraphs      []interface{} `json:"paragraphs"`
	ImagePrompt     string        `json:"imagePrompt"`
}

// contentOutline is the intermediate plan produced by Step 1 (research & outline).
type contentOutline struct {
	Angle       string   `json:"angle"`
	Audience    string   `json:"audience"`
	Category    string   `json:"category"`
	Heading     string   `json:"heading"`
	MetaTitle   string   `json:"metaTitle"`
	MetaDesc    string   `json:"metaDescription"`
	Sections    []string `json:"sections"`
	KeyTerms    []string `json:"keyTerms"`
	ImagePrompt string   `json:"imagePrompt"`
}

// generateBlogPost uses a 2-step agent pipeline with no extra instructions.
func (s *Server) generateBlogPost(ctx context.Context, targetQuery string, impressions int64) (*generatedPost, error) {
	return s.generateBlogPostWithInstructions(ctx, targetQuery, impressions, "")
}

// generateBlogPostWithInstructions uses a 2-step agent pipeline:
//
//	Step 1 — Content Strategist: research the topic, pick an angle, build an outline
//	Step 2 — Content Writer: write the full article from the outline (uses higher-quality model)
//
// customInstructions is optional admin guidance injected into both steps.
func (s *Server) generateBlogPostWithInstructions(ctx context.Context, targetQuery string, impressions int64, customInstructions string) (*generatedPost, error) {

	extraGuidance := ""
	if customInstructions != "" {
		extraGuidance = fmt.Sprintf("\n\nADMIN INSTRUCTIONS (follow these carefully):\n%s\n", customInstructions)
	}

	today := time.Now().Format("2 January 2006")

	// Use blog-specific model for writing (higher quality), fallback to default
	blogModel := s.cfg.OpenAIBlogModel
	if blogModel == "" {
		blogModel = s.cfg.OpenAIModel
	}

	// ── Step 1: Content Strategist — research & outline ──────────────────────

	strategistPrompt := fmt.Sprintf(`You are a content planner at 91Astrology.com, an Indian Vedic astrology website.
Today's date: %s

Plan a blog post for this topic: "%s"
Search impressions: %d

Rules:
1. Pick ONE specific angle — not a broad overview. Example: instead of "all about Ekadashi", pick "why breaking Ekadashi vrat early causes problems" or "5 foods you can eat during Ekadashi fast".
2. The heading must sound like a real Hindi astrology blog title. Short, direct, clickable.
   GOOD: "Ekadashi Ka Vrat Kaise Kholein — Sahi Vidhi", "Shani Sade Sati Mein Kya Karein Kya Na Karein", "Rahu Ketu Transit 2026: Kaun Si Rashi Ko Milega Fayda?"
   BAD: "Unveiling the Mysteries of Ekadashi", "A Comprehensive Guide to Sade Sati", "Harnessing the Power of Rahu Ketu Transit"
3. Plan 5-7 sections. Each section title should be a question or a specific claim, not a generic topic.
   GOOD section: "Vrat kholne ka sahi samay kya hai?" / "Which nakshatra people are most affected?"
   BAD section: "Understanding the Significance" / "The Importance of Rituals"
4. List 6-8 Hindi/Sanskrit terms readers would search for (e.g., "ekadashi vrat vidhi", "parana time", "nirjala ekadashi").
5. Image: describe a warm, realistic Indian spiritual scene (temple, diya, puja thali, etc). No text in image.

LANGUAGE: Write the heading and metaTitle in natural Hinglish (Hindi words in Roman script mixed with English). The metaDescription should be in English for SEO.

DO NOT use any of these words anywhere: unveil, unlock, harness, delve, realm, tapestry, landscape, embark, comprehensive, crucial, fascinating, remarkable, navigate, revolutionary.

Output ONLY valid JSON (no markdown fences):
{
  "angle": "specific angle in 1 sentence",
  "audience": "who will read this and why",
  "category": "Festival|Zodiac|Kundli|Numerology|Vedic|Tarot|Vastu|Palm Reading|National",
  "heading": "Hinglish heading, max 65 chars",
  "metaTitle": "SEO title, max 60 chars, Hinglish",
  "metaDescription": "English meta desc, max 155 chars, include topic + action word",
  "sections": ["Section 1 as question/claim", "Section 2", ...],
  "keyTerms": ["hindi/sanskrit term 1", "term 2", ...],
  "imagePrompt": "realistic Indian spiritual scene description for image generation, warm colors, no text"
}`,
		today,
		targetQuery,
		impressions,
	)

	stratResp, err := s.openai.Client().CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: s.cfg.OpenAIModel,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: "You are a content planner for an Indian Vedic astrology blog. You think like an Indian reader searching Google in Hindi/English. Output only valid JSON."},
			{Role: openai.ChatMessageRoleUser, Content: strategistPrompt + extraGuidance},
		},
		Temperature: 0.5,
	})
	if err != nil {
		return nil, fmt.Errorf("strategist step: %w", err)
	}

	var outline contentOutline
	if err := json.Unmarshal([]byte(cleanLLMJSON(stratResp.Choices[0].Message.Content)), &outline); err != nil {
		return nil, fmt.Errorf("parse outline: %w", err)
	}

	// ── Step 2: Expert Writer — write full article from outline ──────────────

	sectionsJSON, _ := json.Marshal(outline.Sections)
	termsJSON, _ := json.Marshal(outline.KeyTerms)

	writerPrompt := fmt.Sprintf(`You are writing a blog post for 91Astrology.com. Write like a senior Indian astrologer who talks to clients every day — not like a content mill or AI.

Today's date: %s

CONTENT PLAN:
- Topic: "%s"
- Angle: %s
- Reader: %s
- Sections: %s
- Terms to include naturally: %s

═══════════════════════════════════════════════════════════
WRITING RULES — READ EVERY SINGLE ONE BEFORE YOU START
═══════════════════════════════════════════════════════════

LANGUAGE:
- Write the ENTIRE article in ENGLISH. This is non-negotiable.
- You may use well-known Hindi/Sanskrit terms (like Rahu, Ketu, Mahadasha, Kundli, Panchang, Tithi, Nakshatra, Muhurat, Graha, Dasha) but always in Roman script and with a brief explanation on first use.
- Do NOT randomly switch to Hindi sentences mid-paragraph. Do NOT write full Hindi sentences. Do NOT use Devanagari script.
- Example of CORRECT usage: "During Sade Sati (Saturn's 7.5-year transit over your Moon sign), you might feel..."
- Example of WRONG usage: "Sade Sati mein aapko bohot mushkilein aati hain aur yeh samay..."

TONE AND STYLE:
- Write like you are explaining to a friend over chai. Warm, direct, personal.
- Use "you" and "your" frequently. Talk TO the reader.
- Start the article with a specific hook — a real scenario, a bold statement, or a practical question. NOT "Have you ever wondered..." or "In the vast tapestry of..."
- GOOD opening: "If you were born between 1990-1995, chances are you've already gone through your first Saturn return — and you felt it."
- BAD opening: "Vedic astrology has been guiding humanity for thousands of years..."
- Vary paragraph lengths. Some 2-3 sentences, some longer. Never uniform blocks.
- Use short sentences for impact. "That changes everything." / "This is where most people go wrong."

CONTENT QUALITY:
- Every section must teach something specific. No filler paragraphs that just repeat the heading in different words.
- Give SPECIFIC remedies with exact details: which day, which mantra (with text), which gemstone (with weight/metal), which food to donate, etc.
  GOOD: "Chant 'Om Shanaischaraya Namaha' 108 times every Saturday morning before sunrise. Wear a 7-mukhi Rudraksha in a silver pendant."
  BAD: "Chanting mantras and wearing appropriate gemstones can help mitigate the effects."
- When mentioning planetary transits or dates, ONLY mention them if you are 100%% certain. If unsure, describe the general effect without claiming specific dates.
- Do NOT invent transit dates, retrograde periods, or eclipse dates. If the topic is about a specific date/event, the admin instructions will tell you.
- Give real examples: "If your Moon is in Bharani Nakshatra and Mars is in the 7th house, you might notice..."
- Do NOT repeat the same advice for all 12 zodiac signs. If covering multiple signs, give genuinely different predictions/remedies for each.

STRUCTURE:
- Do NOT always follow the "sign-by-sign" format. Vary your article structure based on the topic.
- For festivals/vrats: focus on vidhi (method), timing, do's/don'ts, spiritual significance
- For transits: focus on who's affected most, what to expect, specific remedies
- For remedies: focus on the problem, why it works, step-by-step instructions, common mistakes
- Each section: 150-300 words of real content

CTA — INCLUDE THESE NATURALLY:
- Mention 91Astrology at least once: "You can check your personalized prediction on 91Astrology.com"
- End with a practical next step: "Get your free Kundli on 91Astrology to see exactly how this transit affects your chart."
- Do NOT make CTAs sound salesy. Weave them naturally into advice.

ABSOLUTELY BANNED (using any of these = article rejected):
- Words: delve, tapestry, landscape, realm, embark, unveil, unlock, harness, navigate, comprehensive, crucial, leverage, innovative, cutting-edge, game-changer, revolutionize, furthermore, moreover, nonetheless, fascinating, intriguing, remarkable, pivotal, myriad, plethora, paradigm
- Phrases: "In today's world", "In this article we will", "It's important to note", "It's worth noting", "Without further ado", "At the end of the day", "In conclusion", "Let's dive in", "Have you ever wondered", "Since ancient times", "From time immemorial", "Imagine a world where", "More than just a", "Not just... but also"
- Patterns: Starting 3+ consecutive paragraphs with the same word. Using the word "journey" to describe anything other than actual travel.

TOTAL LENGTH: 1800-2500 words across all sections combined.

═══════════════════════════════════════════════════════════

Output ONLY valid JSON:
{
  "heading": "%s",
  "metaTitle": "%s",
  "metaDescription": "%s",
  "category": "%s",
  "content": [
    {"children": [{"text": "Introduction paragraph — 150-200 words, starts with a hook, sets up what the reader will learn."}]}
  ],
  "paragraphs": [
    {
      "Heading": "Section Title",
      "Paragraph": [{"children": [{"text": "Full section content, 150-300 words, specific details, examples, remedies where relevant."}]}]
    }
  ]
}

Format rules:
- "content" and "Paragraph" values must be Slate rich text: array of objects with "children" array containing {"text": "..."} objects
- Use the exact heading, metaTitle, metaDescription, category values provided above
- Write ALL planned sections — do not skip any
- No markdown inside text values — plain text only`,
		today,
		targetQuery,
		outline.Angle,
		outline.Audience,
		string(sectionsJSON),
		string(termsJSON),
		outline.Heading,
		outline.MetaTitle,
		outline.MetaDesc,
		outline.Category,
	)

	writerResp, err := s.openai.Client().CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: blogModel,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: "You are a senior Vedic astrologer writing blog posts for 91Astrology.com. You write in clear English with Hindi/Sanskrit astrology terms where natural. Your tone is warm, direct, and personal — like talking to a client. You never sound like an AI. You give specific, actionable advice with exact mantras, gemstones, and rituals. Output only valid JSON, no markdown fences."},
			{Role: openai.ChatMessageRoleUser, Content: writerPrompt + extraGuidance},
		},
		Temperature: 1,
	})
	if err != nil {
		return nil, fmt.Errorf("writer step: %w", err)
	}

	var post generatedPost
	if err := json.Unmarshal([]byte(cleanLLMJSON(writerResp.Choices[0].Message.Content)), &post); err != nil {
		return nil, fmt.Errorf("parse blog post: %w", err)
	}

	if post.Heading == "" || post.MetaTitle == "" {
		return nil, fmt.Errorf("generated post has empty heading or title")
	}

	// Ensure every paragraph has image.isHidden=true so the blog page
	// doesn't fall through to rendering the post's featured image per section.
	for i, p := range post.Paragraphs {
		if para, ok := p.(map[string]interface{}); ok {
			if _, hasImage := para["image"]; !hasImage {
				para["image"] = map[string]interface{}{
					"isHidden": true,
				}
				post.Paragraphs[i] = para
			}
		}
	}

	post.ImagePrompt = outline.ImagePrompt
	return &post, nil
}

// cleanLLMJSON strips markdown code fences that LLMs sometimes wrap around JSON output.
func cleanLLMJSON(raw string) string {
	s := strings.TrimSpace(raw)
	// Strip ```json ... ``` or ``` ... ```
	if strings.HasPrefix(s, "```") {
		// Remove opening fence (```json or ```)
		if idx := strings.Index(s, "\n"); idx != -1 {
			s = s[idx+1:]
		}
		// Remove closing fence
		if idx := strings.LastIndex(s, "```"); idx != -1 {
			s = s[:idx]
		}
		s = strings.TrimSpace(s)
	}
	return s
}

// categoryMap caches CMS category name → ID mapping.
// Loaded once per process lifetime.
var (
	categoryMap     map[string]string
	categoryMapOnce sync.Once
	defaultAuthorID string
	authorOnce      sync.Once
)

func (s *Server) getDefaultAuthorAndCategory(ctx context.Context) (string, string) {
	return s.resolveAuthorID(), ""
}

// resolveCategoryID maps a human-readable category name (from LLM) to a CMS category document ID.
func (s *Server) resolveCategoryID(name string) string {
	categoryMapOnce.Do(func() {
		categoryMap = make(map[string]string)
		cats, err := s.cms.ListCategories(30)
		if err != nil {
			log.Printf("[content] WARN: could not fetch categories: %v", err)
			return
		}
		for _, cat := range cats {
			id, _ := cat["id"].(string)
			// Try both "name" and "Name" fields
			n, _ := cat["name"].(string)
			if n == "" {
				n, _ = cat["Name"].(string)
			}
			if id != "" && n != "" {
				categoryMap[strings.ToLower(n)] = id
			}
		}
		log.Printf("[content] Loaded %d category mappings", len(categoryMap))
	})

	if id, ok := categoryMap[strings.ToLower(name)]; ok {
		return id
	}
	// Try partial match
	lower := strings.ToLower(name)
	for k, v := range categoryMap {
		if strings.Contains(k, lower) || strings.Contains(lower, k) {
			return v
		}
	}
	return ""
}

// resolveAuthorID returns the first non-archived English author from CMS.
func (s *Server) resolveAuthorID() string {
	authorOnce.Do(func() {
		authors, err := s.cms.ListAuthors(10)
		if err != nil {
			log.Printf("[content] WARN: could not fetch authors: %v", err)
			return
		}
		for _, a := range authors {
			id, _ := a["id"].(string)
			name, _ := a["name"].(string)
			if name == "" {
				name, _ = a["Name"].(string)
			}
			archived, _ := a["archived"].(bool)
			// Pick the first non-archived author with an English-looking name
			if id != "" && !archived && name != "" && name[0] < 128 {
				defaultAuthorID = id
				log.Printf("[content] Default author: %s (%s)", name, id)
				break
			}
		}
	})
	return defaultAuthorID
}
