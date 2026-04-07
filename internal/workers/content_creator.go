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
//	Step 1 — Senior Content Strategist: research the topic, pick an angle, build an outline
//	Step 2 — Expert Content Writer: write the full article from the outline
//
// customInstructions is optional admin guidance injected into both steps.
func (s *Server) generateBlogPostWithInstructions(ctx context.Context, targetQuery string, impressions int64, customInstructions string) (*generatedPost, error) {

	extraGuidance := ""
	if customInstructions != "" {
		extraGuidance = fmt.Sprintf("\n\nADMIN INSTRUCTIONS (follow these carefully):\n%s\n", customInstructions)
	}

	// ── Step 1: Content Strategist — research & outline ──────────────────────

	strategistPrompt := fmt.Sprintf(`You are a Senior Content Strategist at 91Astrology, India's leading Vedic astrology platform.
You have 15+ years of experience in astrology content that ranks on Google India.

Your task: research and plan a blog post for the keyword below.

Target keyword: "%s"
Monthly search impressions: %d

Think step by step:
1. INTENT ANALYSIS — What is the searcher really looking for? Are they a beginner, intermediate, or advanced reader? What problem are they trying to solve?
2. COMPETITIVE ANGLE — What unique angle can 91Astrology take that generic sites won't? Think Vedic-specific insights, practical remedies, real-life examples.
3. CONTENT STRUCTURE — Plan 5-7 sections that flow logically, each answering a specific sub-question the reader has.
4. KEY TERMS — List 8-10 related Vedic astrology terms (Sanskrit, Hindi, English) to weave in naturally for topical authority.
5. IMAGE CONCEPT — Describe a single featured image that would visually represent this topic (for AI image generation).

HEADING AND TITLE RULES (CRITICAL):
- Write like a human journalist, NOT an AI. The heading must feel like something a real Indian astrology blogger would write.
- NEVER use these AI-sounding patterns: "Unveiled", "Unlocking", "Demystified", "Harnessing", "Comprehensive Guide", "Everything You Need to Know", "The Ultimate", "Dive Into", "Navigating", "Exploring", "Discover the Power of", "A Deep Dive", "Revolutionize", "Game-Changer", "Realm", "Tapestry", "Landscape", "Embark"
- GOOD heading examples: "Lal Kitab Remedies That Actually Work", "Why Your Kundli Might Be Wrong", "Mars in 7th House — What It Means for Marriage", "10 Vastu Tips for a New Home"
- BAD heading examples: "Unveiling the Secrets of Lal Kitab", "Harnessing Cosmic Energy Through Vedic Wisdom", "Exploring the Mystical Realm of Tarot"
- Keep it conversational, specific, and direct. Use numbers, questions, or bold claims.

Output ONLY valid JSON:
{
  "angle": "the unique angle/hook for this article (1-2 sentences)",
  "audience": "who this is for and their knowledge level",
  "category": "one of: Festival, Zodiac, Kundli, Numerology, Vedic, Tarot, Vastu, Palm Reading, National",
  "heading": "natural, human-sounding heading (max 60 chars, include keyword)",
  "metaTitle": "SEO title (max 60 chars, natural tone, include keyword)",
  "metaDescription": "meta description (max 155 chars, conversational, include keyword + CTA)",
  "sections": ["Section 1 Title", "Section 2 Title", ...],
  "keyTerms": ["term1", "term2", ...],
  "imagePrompt": "detailed description for AI image generation: style, colors, subject, mood — Indian/Vedic aesthetic, no text in image"
}`,
		targetQuery,
		impressions,
	)

	stratResp, err := s.openai.Client().CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: s.cfg.OpenAIModel,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: "You are a senior content strategist with deep expertise in Vedic astrology and SEO. You write like a real human blogger — never use AI-sounding words like 'unveil', 'delve', 'realm', 'harness', 'comprehensive guide'. Think carefully before planning. Output only valid JSON, no markdown."},
			{Role: openai.ChatMessageRoleUser, Content: strategistPrompt + extraGuidance},
		},
		Temperature: 0.6,
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

	writerPrompt := fmt.Sprintf(`You are an Expert Vedic Astrology Content Writer with 10+ years of experience writing long-form articles that rank #1 on Google India.

You have been given a detailed content plan by your strategist. Now write the full article.

CONTENT PLAN:
- Target keyword: "%s"
- Angle: %s
- Audience: %s
- Sections to cover: %s
- Key Vedic terms to weave in: %s

WRITING GUIDELINES:
- Write as a knowledgeable friend explaining astrology — warm, authoritative, never condescending
- Open with a hook that connects to the reader's real life (a question, a scenario, a surprising fact)
- Each section should be 150-250 words with concrete details, not generic filler
- Use examples: "For instance, if your Moon is in Rohini Nakshatra..." or "Consider someone born during Amavasya..."
- Include practical takeaways: remedies, mantras, gemstone suggestions, dos/don'ts where relevant
- Naturally weave in the Vedic terms — don't force them, explain briefly when introducing a term
- End with a clear conclusion that gives the reader a next step (check their Kundli, consult an astrologer, etc.)
- Total word count: 1500-2500 words

ANTI-AI LANGUAGE RULES (STRICTLY FOLLOW):
- Write like a real Indian astrology blogger, NOT like an AI assistant
- BANNED words/phrases — NEVER use any of these: "delve", "tapestry", "landscape", "realm", "embark", "unveil", "unlock", "harness", "navigate", "comprehensive", "crucial", "leverage", "innovative", "cutting-edge", "game-changer", "revolutionize", "in today's world", "in the realm of", "it's important to note", "it's worth noting", "in conclusion", "furthermore", "moreover", "nonetheless", "fascinating", "intriguing", "remarkable", "let's dive in", "without further ado", "at the end of the day"
- BANNED sentence patterns: "Imagine a world where...", "Have you ever wondered...", "In this article, we will explore...", "Let's embark on a journey...", "[Topic] is more than just..."
- Instead write like a person who actually practices astrology and talks to clients daily
- Use simple, direct Hindi-English mixed tone where natural (like "Rahu ka prabhav" instead of "the influence of the shadow planet Rahu")
- Vary sentence length — mix short punchy sentences with longer explanations
- Be specific: "wear a 7-mukhi Rudraksha on Thursday" instead of "certain gemstones can help align your energies"

Output ONLY valid JSON:
{
  "heading": "%s",
  "metaTitle": "%s",
  "metaDescription": "%s",
  "category": "%s",
  "content": [
    {"children": [{"text": "Engaging introduction paragraph (150-200 words with hook)..."}]}
  ],
  "paragraphs": [
    {
      "Heading": "Section Title",
      "Paragraph": [{"children": [{"text": "Section content 150-250 words with examples and Vedic terms..."}]}]
    }
  ]
}

CRITICAL RULES:
- Content and Paragraph text MUST be Slate rich text format: array of objects with "children" array containing objects with "text" key
- Use the exact heading, metaTitle, metaDescription, and category from above
- Write ALL sections from the plan — do not skip any
- Every section must have real substance — no thin paragraphs, no generic advice`,
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
		Model: s.cfg.OpenAIModel,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: "You are an expert Vedic astrology content writer who writes like a real Indian astrology blogger. Your writing must sound 100% human — never use AI cliche words like 'unveil', 'delve', 'realm', 'harness', 'navigate', 'comprehensive'. Write in a natural, conversational tone with Hindi-English mix where appropriate. Output only valid JSON, no markdown."},
			{Role: openai.ChatMessageRoleUser, Content: writerPrompt + extraGuidance},
		},
		Temperature: 0.7,
		MaxTokens:   4096,
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
