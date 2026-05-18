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
	"github.com/91astro/seo-agent/internal/services"
	"github.com/hibiken/asynq"

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
	log.Printf("[content-creator] Creating up to %d new blog posts from trending GSC queries...", p.MaxPosts)

	// Pull trending organic queries directly from GSC (last 7d vs prior 7d).
	// This avoids the stale Mongo aggregate which surfaced low-volume gaps.
	trending, err := s.gsc.FetchTrendingQueries(ctx, 7, 30)
	if err != nil {
		return fmt.Errorf("content-creator trending fetch: %w", err)
	}

	type gapCandidate struct {
		Query            string
		TotalImpressions int64
		TotalClicks      int64
		AvgPosition      float64
		GrowthRatio      float64
	}
	var gaps []gapCandidate
	candidateLimit := p.MaxPosts * 10
	for _, t := range trending {
		// Skip queries already ranking well — they don't need a new page.
		if t.AvgPosition > 0 && t.AvgPosition <= 3 {
			continue
		}
		gaps = append(gaps, gapCandidate{
			Query:            t.Query,
			TotalImpressions: t.RecentImpressions,
			TotalClicks:      t.RecentClicks,
			AvgPosition:      t.AvgPosition,
			GrowthRatio:      t.GrowthRatio,
		})
		if len(gaps) >= candidateLimit {
			break
		}
	}

	// Fetch existing posts to avoid duplicates and count per-category distribution
	existingPosts, err := s.cms.ListPosts(500, "en")
	if err != nil {
		log.Printf("[content-creator] WARN: could not list existing posts: %v", err)
	}
	existingHeadings := map[string]bool{}
	var existingHeadingsList []string
	categoryIDCounts := map[string]int{}
	for _, post := range existingPosts {
		if h, ok := post["Heading"].(string); ok {
			existingHeadings[strings.ToLower(h)] = true
			existingHeadingsList = append(existingHeadingsList, h)
		}
		if catID, ok := post["category"].(string); ok && catID != "" {
			categoryIDCounts[catID]++
		}
	}

	// Fetch real categories from CMS and build name→count map using CMS IDs
	cmsCategoryMap := s.fetchCMSCategories()
	var cmsCategories []string
	categoryCounts := map[string]int{}
	for _, cat := range cmsCategoryMap {
		cmsCategories = append(cmsCategories, cat.Name)
		categoryCounts[strings.ToLower(cat.Name)] = categoryIDCounts[cat.ID]
	}
	log.Printf("[content-creator] CMS categories: %v", cmsCategories)

	// Cluster key dedup — avoid generating posts for queries that already have coverage
	usedClusterKeys := s.loadUsedClusterKeysFromCMS()
	log.Printf("[content-creator] %d cluster keys already used", len(usedClusterKeys))

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

		// Cluster key dedup — skip queries whose topic cluster is already covered
		clusterKey := services.ClusterKey(gap.Query)
		if usedClusterKeys[clusterKey] {
			continue
		}

		log.Printf("[content-creator] ── Trending: %q (%d impr, pos %.1f, growth %+.0f%%) ──",
			gap.Query, gap.TotalImpressions, gap.AvgPosition, gap.GrowthRatio*100)

		// Generate blog post via GPT-4o
		post, err := s.generateBlogPostAware(ctx, gap.Query, gap.TotalImpressions, existingHeadingsList, cmsCategories, categoryCounts)
		if err != nil {
			log.Printf("[content-creator]   ✗ generation failed: %v", err)
			continue
		}

		// Check for duplicate heading
		if existingHeadings[strings.ToLower(post.Heading)] {
			log.Printf("[content-creator]   ⊘ skip — heading already exists: %q", post.Heading)
			continue
		}

		// Featured image — CMS Posts requires it. Fall back to a heading-derived
		// prompt if the strategist omitted imagePrompt, and skip the post if
		// generation/upload fails.
		imagePrompt := post.ImagePrompt
		if imagePrompt == "" {
			imagePrompt = fmt.Sprintf("Warm realistic Indian spiritual scene illustrating: %s. Soft golden lighting, temple or puja setting, no text.", post.Heading)
			log.Printf("[content-creator]   ~ strategist omitted imagePrompt — using heading-based fallback")
		}
		imageID, err := s.generateAndUploadImage(ctx, imagePrompt, post.Heading)
		if err != nil {
			log.Printf("[content-creator]   ✗ image generation failed, skipping post (CMS requires image): %v", err)
			continue
		}
		log.Printf("[content-creator]   + Featured image uploaded: %s", imageID)

		categoryRelID := s.resolveCategoryID(post.Category)
		authorID := s.resolveAuthorID()

		// Create post in CMS
		cmsPost := map[string]interface{}{
			"title":      post.Heading,
			"Heading":    post.Heading,
			"Date":       time.Now().Format("2 January 2006"),
			"Category":   post.Category,
			"Content":    post.Content,
			"Paragraph":  post.Paragraphs,
			"Identifier": "en",
			"image":      imageID,
			"isHidden":   true, // start hidden — human reviews before publishing
			"meta": map[string]string{
				"title":       post.MetaTitle,
				"description": post.MetaDescription,
			},
		}
		if authorID != "" {
			cmsPost["author"] = authorID
		}
		if categoryRelID != "" {
			cmsPost["category"] = categoryRelID
		}

		docID, err := s.cms.CreatePost(cmsPost)
		if err != nil {
			log.Printf("[content-creator]   ✗ CMS create failed: %v", err)
			continue
		}

		log.Printf("[content-creator]   ✓ Created post %s: %q (hidden, needs review)", docID, post.Heading)

		usedClusterKeys[clusterKey] = true
		existingHeadings[strings.ToLower(post.Heading)] = true

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
	return s.generateBlogPostFull(ctx, targetQuery, impressions, "", nil, nil, nil, nil)
}

// generateBlogPostAware uses a 2-step agent pipeline, passing existing headings and category distribution to avoid duplication.
func (s *Server) generateBlogPostAware(ctx context.Context, targetQuery string, impressions int64, existingHeadings []string, cmsCategories []string, categoryCounts map[string]int) (*generatedPost, error) {
	return s.generateBlogPostFull(ctx, targetQuery, impressions, "", existingHeadings, cmsCategories, categoryCounts, nil)
}

// generateBlogPostEnriched is the same as Aware but with current-moment research
// (festivals, transits, celebrity news) layered into the strategist prompt.
func (s *Server) generateBlogPostEnriched(ctx context.Context, targetQuery string, impressions int64, existingHeadings []string, cmsCategories []string, categoryCounts map[string]int, enrich *ThemeEnrichment) (*generatedPost, error) {
	return s.generateBlogPostFull(ctx, targetQuery, impressions, "", existingHeadings, cmsCategories, categoryCounts, enrich)
}

// generateBlogPostWithInstructions uses a 2-step agent pipeline with custom admin instructions.
func (s *Server) generateBlogPostWithInstructions(ctx context.Context, targetQuery string, impressions int64, customInstructions string) (*generatedPost, error) {
	return s.generateBlogPostFull(ctx, targetQuery, impressions, customInstructions, nil, nil, nil, nil)
}

// generateBlogPostFull uses a 2-step agent pipeline:
//
//	Step 1 — Content Strategist: research the topic, pick an angle, build an outline
//	Step 2 — Content Writer: write the full article from the outline (uses higher-quality model)
func (s *Server) generateBlogPostFull(ctx context.Context, targetQuery string, impressions int64, customInstructions string, existingHeadings []string, cmsCategories []string, categoryCounts map[string]int, enrich *ThemeEnrichment) (*generatedPost, error) {

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

	// Build existing-headings context for dedup (show last 50 to keep prompt size reasonable)
	existingHeadingsBlock := ""
	if len(existingHeadings) > 0 {
		headingsToShow := existingHeadings
		if len(headingsToShow) > 50 {
			headingsToShow = headingsToShow[len(headingsToShow)-50:]
		}
		existingHeadingsBlock = "\n\nALREADY PUBLISHED TITLES (do NOT repeat these topics, angles, or similar headings):\n"
		for _, h := range headingsToShow {
			existingHeadingsBlock += fmt.Sprintf("- %s\n", h)
		}
	}

	// Build category distribution context so LLM picks underserved categories
	categoryBlock := ""
	if len(cmsCategories) > 0 {
		categoryBlock = "\n\nAVAILABLE CATEGORIES (with current post count — prefer categories with fewer posts):\n"
		for _, cat := range cmsCategories {
			count := categoryCounts[strings.ToLower(cat)]
			categoryBlock += fmt.Sprintf("- %s (%d posts)\n", cat, count)
		}
		categoryBlock += "\nYou MUST pick one of the categories listed above. Prefer categories with fewer posts to balance coverage across the site.\n"
	}

	// ── Step 1: Content Strategist — research & outline ──────────────────────

	strategistPrompt := fmt.Sprintf(`You are a content planner at 91Astrology.com, an Indian Vedic astrology website.
Today's date: %s

Plan a blog post for this topic: "%s"
Search impressions: %d

Rules:
1. Pick ONE specific, UNIQUE angle — not a broad overview. If a list of already-published titles is provided below, you MUST choose a completely different angle that has zero overlap with any existing post. Think about what aspect of this topic has NOT been covered yet.
   Example: instead of "all about Ekadashi", pick "why breaking Ekadashi vrat early causes problems" or "5 foods you can eat during Ekadashi fast".

2. THE HEADING IS THE MOST IMPORTANT FIELD. It MUST contain at least ONE of these viral-hook elements (preferably two):
   (a) A NUMBER — "3 mistakes", "5 din mein", "7 cheezein", "₹10,000 ka nuksaan"
   (b) A NAMED PERSON in current news — Bollywood actor, cricketer, politician (only if their kundli/event ties to the topic)
   (c) AN URGENCY WINDOW — "is hafte", "11 May se 14 May tak", "agle 7 din", "before purnima", a specific date from the CURRENT-MOMENT RESEARCH below
   (d) A CONTRARIAN CLAIM — "why most people get this wrong", "the one transit nobody talks about", "yeh galti mat karein", "jo astrologers nahi batate"
   (e) A SPECIFIC OUTCOME — "career milega boost", "shaadi tutegi", "paisa aayega", "naukri jaayegi", "love life mein twist"

   If CURRENT-MOMENT RESEARCH provides a recommendedDateHook, the heading MUST anchor to that exact date or window.

   GOOD headings (note the hook):
   - "11 May Se Mangal Mesh Mein: In 4 Rashiyon Ko Milega Career Boost"   ← date + number + outcome
   - "Shani Sade Sati: Yeh 3 Galtiyaan Mat Karna Warna Paisa Khatam"     ← number + contrarian + outcome
   - "Akshaya Tritiya 2026: Sona Kharidne Se Pehle Yeh Ek Cheez Padh Lo" ← date + contrarian
   - "Harbhajan Singh Ki Kundli: 12 May Ko Kya Hone Wala Hai?"          ← named person + date + outcome
   - "Is Hafte Mokor Rashi Walon Ke Liye 5 Din Hain Sabse Bhaari"        ← urgency + number + outcome

   BAD headings (these get the post REJECTED — bland, descriptive, no hook):
   - "Mokor Rashi Ke Liye Akshaya Tritiya Kaise Laaye Dhan Labh"  ← descriptive how-to, no hook, no urgency
   - "Chaitra Purnima Par Makar Rashi Kaise Karein Vikas"         ← descriptive, no number/outcome/specifics
   - "Ekadashi Ka Vrat Kaise Kholein — Sahi Vidhi"                ← evergreen how-to, no hook
   - "Understanding the Significance of Sade Sati"                ← generic explainer
   - Any heading starting with "Kaise [verb]" without a number, date, or named person elsewhere in the title

   BANNED HEADING PATTERNS (do not use these as a template):
   - "X kaise laaye Y" / "X kaise karein Y" — these are descriptive, not viral
   - "Sahi vidhi", "complete guide", "everything you need to know"
   - Heading without a single number, date, named person, or specific outcome

3. Plan 5-7 sections. Each section title should be a question or a specific claim, not a generic topic. At least ONE section title must explicitly reference the date/event from CURRENT-MOMENT RESEARCH.
   GOOD section: "11 May ke baad Mokor Rashi walon ko kya karna chahiye?" / "Kaunsi 4 rashiyon par sabse zyada asar?"
   BAD section: "Understanding the Significance" / "The Importance of Rituals"

4. List 6-8 Hindi/Sanskrit terms readers would search for (e.g., "ekadashi vrat vidhi", "parana time", "nirjala ekadashi").
5. Image prompt: Write a DALL-E prompt for a unique featured image that is SPECIFIC to the blog topic. The image must visually represent the actual subject of the article — not a generic spiritual scene.
   - If the topic is about a zodiac sign → show its symbol, constellation, or associated imagery
   - If the topic is about a planet (Shani, Rahu, etc.) → show the planet, its yantra, or cosmic imagery
   - If the topic is about a festival → show the specific festival scene (e.g., Holi colors, Diwali fireworks, Navratri garba)
   - If the topic is about palmistry → show hands with lines highlighted
   - If the topic is about vastu → show architectural/home layout imagery
   - If the topic is about numerology → show numbers with mystical/cosmic styling
   - NEVER default to a generic diya, puja thali, or temple scene unless the article is specifically about puja or temple worship
   - Style: cinematic lighting, rich colors, photorealistic or high-quality digital art. No text or watermarks in the image.

LANGUAGE: Write the heading and metaTitle in natural Hinglish (Hindi words in Roman script mixed with English). The metaDescription should be in English for SEO.

DO NOT use any of these words anywhere: unveil, unlock, harness, delve, realm, tapestry, landscape, embark, comprehensive, crucial, fascinating, remarkable, navigate, revolutionary.

Output ONLY valid JSON (no markdown fences):
{
  "angle": "specific angle in 1 sentence",
  "audience": "who will read this and why",
  "category": "one of the available categories listed below (or best fit if no list provided)",
  "heading": "Hinglish heading, max 65 chars",
  "metaTitle": "SEO title, max 60 chars, Hinglish",
  "metaDescription": "English meta desc, max 155 chars, include topic + action word",
  "sections": ["Section 1 as question/claim", "Section 2", ...],
  "keyTerms": ["hindi/sanskrit term 1", "term 2", ...],
  "imagePrompt": "detailed DALL-E prompt specific to the blog topic — describe the exact visual scene, subject matter, composition, and art style. No generic spiritual imagery. No text in image."
}`,
		today,
		targetQuery,
		impressions,
	)

	stratResp, err := s.openai.Client().CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: s.cfg.OpenAIModel,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: "You are a content planner for an Indian Vedic astrology blog. You think like a viral Indian content creator on YouTube/Instagram, not like a textbook author. Every heading must have a sharp viral hook (number, named person, urgency date, contrarian claim, or specific outcome) — descriptive 'how to' titles are rejected. Output only valid JSON."},
			{Role: openai.ChatMessageRoleUser, Content: strategistPrompt + renderEnrichmentBlock(enrich) + categoryBlock + existingHeadingsBlock + extraGuidance},
		},
		Temperature: 0.7,
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

type cmsCategory struct {
	ID   string
	Name string
}

// fetchCMSCategories returns all categories from CMS with their IDs and names.
func (s *Server) fetchCMSCategories() []cmsCategory {
	cats, err := s.cms.ListCategories(30)
	if err != nil {
		log.Printf("[content] WARN: could not fetch categories: %v", err)
		return nil
	}
	var result []cmsCategory
	for _, cat := range cats {
		id, _ := cat["id"].(string)
		n, _ := cat["name"].(string)
		if n == "" {
			n, _ = cat["Name"].(string)
		}
		if id != "" && n != "" {
			result = append(result, cmsCategory{ID: id, Name: n})
		}
	}
	return result
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
