package workers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	openai "github.com/sashabaranov/go-openai"
	"github.com/91astro/seo-agent/internal/services"
)

type calendarBlogPayload struct {
	MaxPosts int `json:"max_posts"`
}

type upcomingEvent struct {
	Title    string
	Date     string
	Query    string
	Category string
}

// todayFestival checks calendar-bharat for any festival today or tomorrow.
func todayFestival(today time.Time) *upcomingEvent {
	url := fmt.Sprintf("https://jayantur13.github.io/calendar-bharat/calendar/%d.json", today.Year())
	resp, err := http.Get(url)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var raw map[string]map[string]map[string]struct {
		Event  string `json:"event"`
		Type   string `json:"type"`
		Extras string `json:"extras"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}

	// Check today and tomorrow
	for _, d := range []time.Time{today, today.AddDate(0, 0, 1)} {
		for _, monthEntries := range raw[fmt.Sprintf("%d", d.Year())] {
			for dateStr, entry := range monthEntries {
				parts := strings.Split(dateStr, ", ")
				if len(parts) < 3 {
					continue
				}
				evDate, err := time.Parse("January 2, 2006", parts[0]+", "+parts[1])
				if err != nil || evDate.Format("2006-01-02") != d.Format("2006-01-02") {
					continue
				}
				// Skip non-religious and non-astrology entries
				if strings.ToLower(entry.Type) == "good to know" {
					continue
				}
				if strings.Contains(strings.ToLower(entry.Type), "government") {
					continue
				}
				name := strings.ToLower(entry.Event)
				if strings.Contains(name, "mother's day") || strings.Contains(name, "father's day") ||
					strings.Contains(name, "valentine") || strings.Contains(name, "christmas") ||
					strings.Contains(name, "new year") || strings.Contains(name, "world ") ||
					strings.Contains(name, "international ") {
					continue
				}
				return &upcomingEvent{
					Title:    fmt.Sprintf("%s %d", strings.TrimSpace(entry.Event), d.Year()),
					Date:     d.Format("2006-01-02"),
					Query:    fmt.Sprintf("%s %d astrology significance", strings.ToLower(strings.TrimSpace(entry.Event)), d.Year()),
					Category: "Festival",
				}
			}
		}
	}
	return nil
}

// todayTithi fetches today's tithi from Prokerala (1 API call).
func (s *Server) todayTithi(today time.Time) *upcomingEvent {
	if !s.prokerala.IsConfigured() {
		return nil
	}
	anchor, err := s.prokerala.FetchTodayTithi(today)
	if err != nil {
		log.Printf("[calendar-blog] Prokerala tithi fetch failed: %v", err)
		return nil
	}

	significant := map[string]struct{ query, category string }{
		"Shukla Paksha Ekadashi":    {"ekadashi vrat fasting significance", "Vedic"},
		"Krishna Paksha Ekadashi":   {"ekadashi vrat fasting significance", "Vedic"},
		"Shukla Paksha Purnima":     {"purnima significance vedic rituals", "Vedic"},
		"Krishna Paksha Amavasya":   {"amavasya significance ancestors rituals", "Vedic"},
		"Shukla Paksha Trayodashi":  {"pradosh vrat shiva puja significance", "Vedic"},
		"Krishna Paksha Trayodashi": {"pradosh vrat shiva puja significance", "Vedic"},
		"Krishna Paksha Chaturdashi":{"masik shivratri puja significance", "Vedic"},
		"Shukla Paksha Chaturthi":   {"vinayaka chaturthi puja significance", "Festival"},
		"Shukla Paksha Navami":      {"navami significance vedic puja", "Festival"},
	}

	info, ok := significant[anchor.TithiName]
	if !ok {
		log.Printf("[calendar-blog] Tithi %q not significant, skipping", anchor.TithiName)
		return nil
	}

	month := today.Format("January")
	year := today.Year()
	// Extract short name from tithi (e.g. "Shukla Paksha Ekadashi" → "Ekadashi Vrat")
	shortNames := map[string]string{
		"Shukla Paksha Ekadashi": "Ekadashi Vrat", "Krishna Paksha Ekadashi": "Ekadashi Vrat",
		"Shukla Paksha Purnima": "Purnima", "Krishna Paksha Amavasya": "Amavasya",
		"Shukla Paksha Trayodashi": "Pradosh Vrat", "Krishna Paksha Trayodashi": "Pradosh Vrat",
		"Krishna Paksha Chaturdashi": "Masik Shivratri", "Shukla Paksha Chaturthi": "Vinayaka Chaturthi",
		"Shukla Paksha Navami": "Navami",
	}
	name := shortNames[anchor.TithiName]

	return &upcomingEvent{
		Title:    fmt.Sprintf("%s %s %d", name, month, year),
		Date:     today.Format("2006-01-02"),
		Query:    fmt.Sprintf("%s %s %d", info.query, strings.ToLower(month), year),
		Category: info.category,
	}
}

// trendingAstroTopic asks OpenAI for one high-traffic astrology topic for today.
func (s *Server) trendingAstroTopic(ctx context.Context, today time.Time) *upcomingEvent {
	prompt := fmt.Sprintf(`Today is %s. Suggest ONE high-traffic Vedic astrology or Indian spirituality topic that people are searching for RIGHT NOW — seasonal, timely, or evergreen with high organic potential.

NOT a festival or tithi (those are covered separately). Think: zodiac predictions, kundli tips, vastu, numerology, palmistry, remedies, planetary effects, nakshatra insights.

Return ONLY valid JSON (single object):
{"title":"Blog title with year","query":"seo target query 3-5 words","category":"Zodiac|Kundli|Vedic|Numerology|Vastu|Palm Reading"}`,
		today.Format("2 January 2006"),
	)

	resp, err := s.openai.Client().CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: s.cfg.OpenAIModel,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleUser, Content: prompt},
		},
		Temperature: 0.7,
	})
	if err != nil {
		log.Printf("[calendar-blog] Trending topic fetch failed: %v", err)
		return nil
	}

	var result struct {
		Title    string `json:"title"`
		Query    string `json:"query"`
		Category string `json:"category"`
	}
	if err := json.Unmarshal([]byte(cleanLLMJSON(resp.Choices[0].Message.Content)), &result); err != nil {
		log.Printf("[calendar-blog] Trending topic parse failed: %v", err)
		return nil
	}

	return &upcomingEvent{
		Title:    result.Title,
		Date:     today.Format("2006-01-02"),
		Query:    result.Query,
		Category: result.Category,
	}
}

// viralAstroTopic asks OpenAI for a celebrity/viral/pop-culture astrology topic
// that drives high organic search traffic — birth charts, predictions, zodiac
// compatibility of famous couples, trending Bollywood/cricket personalities, etc.
func (s *Server) viralAstroTopic(ctx context.Context, today time.Time, avoid []string) *upcomingEvent {
	avoidList := ""
	if len(avoid) > 0 {
		avoidList = fmt.Sprintf("\n\nDO NOT suggest any of these topics (already covered): %s", strings.Join(avoid, ", "))
	}

	prompt := fmt.Sprintf(`Today is %s. Suggest ONE viral, high-search-volume astrology topic about a REAL celebrity, public figure, or pop-culture moment that Indian audiences are actively searching for.

Pick from these categories (rotate, don't always pick the same type):

1. CELEBRITY KUNDLI ANALYSIS — Birth chart breakdown of a trending Bollywood actor, cricketer, politician, or influencer. Pick someone in the news RIGHT NOW or with an upcoming birthday/movie/match.
   Examples: "Shah Rukh Khan Ki Kundli — Raj Yoga Ka Raaz", "Virat Kohli Birth Chart: Why 2026 Is His Year", "Alia Bhatt Zodiac Sign and Nakshatra Analysis"

2. CELEBRITY COUPLE COMPATIBILITY — Zodiac/kundli matching of a famous couple (married, dating, or rumored).
   Examples: "Ranbir-Alia Kundli Milan: Kitna Compatible Hai Ye Jodi?", "Deepika-Ranveer Zodiac Compatibility Decoded"

3. ZODIAC SIGN LISTICLES — Fun, shareable zodiac content that goes viral on social media.
   Examples: "Most Successful Zodiac Signs in Bollywood", "Which Rashi Makes the Best Life Partner?", "Zodiac Signs Who Become Rich After 30"

4. PREDICTION/CONTROVERSY — Astrology take on a trending news event, election, IPL season, movie release, or viral moment.
   Examples: "IPL 2026: Which Captain's Stars Are Strongest?", "New PM Prediction According to Vedic Astrology", "Why Bollywood Flops Are Connected to Rahu Transit"

5. RELATABLE ASTROLOGY — Everyday life topics through an astrology lens that people Google.
   Examples: "Shadi Ke Liye Sabse Acchi Rashi Kaun Si Hai?", "Career Change Kab Karein — Kundli Se Jaanein", "Love Marriage vs Arranged Marriage: Kya Kehti Hai Kundli?"

RULES:
- The person/topic MUST be real and currently relevant (2025-2026). No fictional characters.
- Title should be in Hinglish (Hindi in Roman script + English mix) — this is what Indians actually search.
- Query should be the Google search terms people would type.
- Pick someone/something that is genuinely trending or evergreen popular — not obscure.%s

Return ONLY valid JSON (single object):
{"title":"Catchy Hinglish blog title","query":"google search query 3-6 words","category":"Zodiac|Kundli|Vedic|Numerology"}`,
		today.Format("2 January 2006"),
		avoidList,
	)

	resp, err := s.openai.Client().CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: s.cfg.OpenAIModel,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleUser, Content: prompt},
		},
		Temperature: 0.9,
	})
	if err != nil {
		log.Printf("[calendar-blog] Viral topic fetch failed: %v", err)
		return nil
	}

	var result struct {
		Title    string `json:"title"`
		Query    string `json:"query"`
		Category string `json:"category"`
	}
	if err := json.Unmarshal([]byte(cleanLLMJSON(resp.Choices[0].Message.Content)), &result); err != nil {
		log.Printf("[calendar-blog] Viral topic parse failed: %v", err)
		return nil
	}

	return &upcomingEvent{
		Title:    result.Title,
		Date:     today.Format("2006-01-02"),
		Query:    result.Query,
		Category: result.Category,
	}
}

func (s *Server) handleCalendarBlogCreate(ctx context.Context, task *asynq.Task) error {
	var p calendarBlogPayload
	if err := json.Unmarshal(task.Payload(), &p); err != nil || p.MaxPosts == 0 {
		p.MaxPosts = 3
	}

	log.Println("[calendar-blog] ─────────────────────────────────────────")

	now := time.Now().UTC()

	// Collect up to 3 events: festival + tithi + trending topic
	var candidates []*upcomingEvent

	if festival := todayFestival(now); festival != nil {
		log.Printf("[calendar-blog] Festival: %s", festival.Title)
		candidates = append(candidates, festival)
	}

	if tithi := s.todayTithi(now); tithi != nil {
		log.Printf("[calendar-blog] Tithi: %s", tithi.Title)
		candidates = append(candidates, tithi)
	}

	// Fill remaining slots alternating: viral (celebrity/pop-culture), trending (astro)
	fillNeeded := p.MaxPosts - len(candidates)
	if fillNeeded < 1 {
		fillNeeded = 1
	}
	var usedTitles []string
	for _, c := range candidates {
		usedTitles = append(usedTitles, c.Title)
	}

	isDup := func(ev *upcomingEvent) bool {
		for _, c := range candidates {
			if strings.EqualFold(c.Query, ev.Query) {
				return true
			}
		}
		return false
	}

	for i := 0; i < fillNeeded; i++ {
		var ev *upcomingEvent
		if i%2 == 0 {
			// Even slots: viral/celebrity topic
			ev = s.viralAstroTopic(ctx, now, usedTitles)
		} else {
			// Odd slots: trending astro topic
			ev = s.trendingAstroTopic(ctx, now)
		}
		if ev != nil && !isDup(ev) {
			label := "Viral"
			if i%2 != 0 {
				label = "Trending"
			}
			log.Printf("[calendar-blog] %s #%d: %s", label, i+1, ev.Title)
			candidates = append(candidates, ev)
			usedTitles = append(usedTitles, ev.Title)
		}
	}

	if len(candidates) == 0 {
		log.Println("[calendar-blog] No candidates today — nothing to generate")
		return nil
	}

	// Dedup against already-published cluster keys
	usedClusterKeys := s.loadUsedClusterKeysFromCMS()
	existingPosts, _ := s.cms.ListPosts(500, "en")
	existingHeadings := map[string]bool{}
	for _, post := range existingPosts {
		if h, ok := post["Heading"].(string); ok {
			existingHeadings[strings.ToLower(h)] = true
		}
	}

	created := 0
	for _, ev := range candidates {
		if created >= p.MaxPosts {
			break
		}

		clusterKey := services.ClusterKey(ev.Query)
		if usedClusterKeys[clusterKey] {
			log.Printf("[calendar-blog]   ~ skip %q — cluster key already used", ev.Title)
			continue
		}

		log.Printf("[calendar-blog] ── Generating: %q ──", ev.Title)

		customInstructions := fmt.Sprintf(
			"Write about '%s'. Today is %s. Make it timely and actionable.",
			ev.Title, now.Format("2 January 2006"),
		)

		post, err := s.generateBlogPostWithInstructions(ctx, ev.Query, 0, customInstructions)
		if err != nil {
			log.Printf("[calendar-blog]   x generation failed: %v", err)
			continue
		}

		if existingHeadings[strings.ToLower(post.Heading)] {
			log.Printf("[calendar-blog]   ~ skip — heading exists: %q", post.Heading)
			continue
		}

		var imageID string
		if post.ImagePrompt != "" {
			if imgID, err := s.generateAndUploadImage(ctx, post.ImagePrompt, post.Heading); err != nil {
				log.Printf("[calendar-blog]   ! image failed: %v", err)
			} else {
				imageID = imgID
			}
		}

		cmsPost := map[string]interface{}{
			"title": post.Heading, "Heading": post.Heading,
			"Date": now.Format("2 January 2006"), "Category": ev.Category,
			"Content": post.Content, "Paragraph": post.Paragraphs,
			"Identifier": "en", "isHidden": true,
			"meta": map[string]string{"title": post.MetaTitle, "description": post.MetaDescription},
		}
		if imageID != "" { cmsPost["image"] = imageID }
		if id := s.resolveAuthorID(); id != "" { cmsPost["author"] = id }
		if id := s.resolveCategoryID(ev.Category); id != "" { cmsPost["category"] = id }

		docID, err := s.cms.CreatePost(cmsPost)
		if err != nil {
			log.Printf("[calendar-blog]   x CMS create failed: %v", err)
			continue
		}
		log.Printf("[calendar-blog]   + Created: %s %q", docID, post.Heading)

		s.cms.CreateTopic(map[string]interface{}{
			"query": ev.Query, "clusterKey": clusterKey,
			"heading": post.Heading, "post": docID, "status": "pending",
		})

		usedClusterKeys[clusterKey] = true
		existingHeadings[strings.ToLower(post.Heading)] = true
		created++
	}

	log.Printf("[calendar-blog] ── Done: %d posts created ──", created)
	log.Println("[calendar-blog] ─────────────────────────────────────────")
	return nil
}
