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
	MaxPosts      int `json:"max_posts"`
	LookAheadDays int `json:"look_ahead_days"`
}

type upcomingEvent struct {
	Title    string `json:"title"`
	Date     string `json:"date"`
	Query    string `json:"query"`
	Category string `json:"category"`
}

// fetchCalendarBharatEvents fetches festival/event data from the calendar-bharat
// GitHub project which has accurate tithi-based dates for each year.
func fetchCalendarBharatEvents(year int, from, to time.Time) ([]upcomingEvent, error) {
	url := fmt.Sprintf("https://jayantur13.github.io/calendar-bharat/calendar/%d.json", year)
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetch calendar-bharat %d: %w", year, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("calendar-bharat %d returned %d", year, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Schema: { "2026": { "January 2026": { "January 1, 2026, Thursday": { event, type, extras } } } }
	var raw map[string]map[string]map[string]struct {
		Event  string `json:"event"`
		Type   string `json:"type"`
		Extras string `json:"extras"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse calendar-bharat: %w", err)
	}

	yearData := raw[fmt.Sprintf("%d", year)]

	var events []upcomingEvent
	for _, monthEntries := range yearData {
		for dateStr, entry := range monthEntries {
			// dateStr format: "January 1, 2026, Thursday"
			// Parse by stripping the day-of-week suffix
			parts := strings.Split(dateStr, ", ")
			if len(parts) < 3 {
				continue
			}
			datePart := parts[0] + ", " + parts[1] // "January 1, 2026"
			evDate, err := time.Parse("January 2, 2006", datePart)
			if err != nil {
				continue
			}

			// Only include events strictly after today and within the look-ahead window
			if !evDate.After(from) || evDate.After(to) {
				continue
			}

			// Skip non-astrology content: government holidays, awareness days
			typeLower := strings.ToLower(entry.Type)
			if strings.Contains(typeLower, "government") {
				continue
			}
			eventLower := strings.ToLower(entry.Event)
			if strings.Contains(eventLower, "world ") ||
				strings.Contains(eventLower, "international ") ||
				strings.Contains(eventLower, "day") && strings.Contains(entry.Extras, "fixed day in Gregorian") {
				continue
			}

			// Map category
			category := "Festival"
			extrasLower := strings.ToLower(entry.Extras)
			if strings.Contains(extrasLower, "astronomy event") ||
				strings.Contains(extrasLower, "retrograde") ||
				strings.Contains(extrasLower, "eclipse") ||
				strings.Contains(extrasLower, "equinox") ||
				strings.Contains(extrasLower, "solstice") {
				category = "Vedic"
			}

			title := fmt.Sprintf("%s %d", strings.TrimSpace(entry.Event), year)
			query := fmt.Sprintf("%s %d astrology significance", strings.ToLower(strings.TrimSpace(entry.Event)), year)

			events = append(events, upcomingEvent{
				Title:    title,
				Date:     evDate.Format("2006-01-02"),
				Query:    query,
				Category: category,
			})
		}
	}
	return events, nil
}

// fetchProkeralaEvents calls Prokerala for each day in the window and returns
// astrologically significant days (Ekadashi, Pradosh, Purnima, Amavasya etc.)
// based on the actual tithi for that date.
func (s *Server) fetchProkeralaEvents(from, to time.Time, existingTitles map[string]bool) []upcomingEvent {
	if !s.prokerala.IsConfigured() {
		log.Println("[calendar-blog] Prokerala not configured — skipping")
		return nil
	}

	var events []upcomingEvent
	seen := map[string]bool{} // deduplicate same tithi-event in window

	for d := from.AddDate(0, 0, 1); !d.After(to); d = d.AddDate(0, 0, 1) {
		panchang, err := s.prokerala.FetchPanchang(d)
		if err != nil {
			log.Printf("[calendar-blog] Prokerala %s: %v", d.Format("2006-01-02"), err)
			continue
		}

		if info, ok := services.SignificantTithis[panchang.Tithi]; ok {
			key := info.Name + d.Format("2006-01")
			if seen[key] || existingTitles[strings.ToLower(info.Name)] {
				continue
			}
			seen[key] = true
			year := d.Year()
			month := d.Format("January")
			events = append(events, upcomingEvent{
				Title:    fmt.Sprintf("%s %s %d", info.Name, month, year),
				Date:     d.Format("2006-01-02"),
				Query:    fmt.Sprintf("%s %s %d", info.Query, strings.ToLower(month), year),
				Category: info.Category,
			})
		}
		// Rate limit: Prokerala free tier is 5 req/min
		time.Sleep(15 * time.Second)
	}
	return events
}

// fetchOpenAIGapEvents asks OpenAI (with web search) for celestial events
// NOT typically covered by festival calendars: retrogrades, eclipses, transits.
func (s *Server) fetchOpenAIGapEvents(ctx context.Context, from, to time.Time, existingTitles map[string]bool) ([]upcomingEvent, error) {
	prompt := fmt.Sprintf(`Search the web for Hindu/Vedic astrological celestial events between %s and %s that are NOT regular festivals.

Focus ONLY on:
- Planetary retrogrades (Saturn, Jupiter, Mercury, Venus, Mars)
- Solar and Lunar eclipses (Surya Grahan, Chandra Grahan)
- Major planetary transits (e.g. Jupiter entering a new sign)
- Rare astronomical events relevant to Vedic astrology

Do NOT include regular festivals or Jayantis — only celestial/astronomical events.
Only include events with dates strictly after %s.

Return ONLY a valid JSON array:
[{"title":"event name with year","date":"YYYY-MM-DD","query":"vedic seo query 3-6 words with year","category":"Vedic"}]`,
		from.Format("2006-01-02"),
		to.Format("2006-01-02"),
		from.Format("2006-01-02"),
	)

	resp, err := s.openai.Client().CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: "gpt-4o-search-preview",
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleUser, Content: prompt},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("openai gap events: %w", err)
	}

	var events []upcomingEvent
	if err := json.Unmarshal([]byte(cleanLLMJSON(resp.Choices[0].Message.Content)), &events); err != nil {
		return nil, fmt.Errorf("parse gap events: %w", err)
	}

	// Filter: only future, only non-duplicate titles
	var filtered []upcomingEvent
	for _, ev := range events {
		evDate, err := time.Parse("2006-01-02", ev.Date)
		if err != nil || !evDate.After(from) {
			continue
		}
		if existingTitles[strings.ToLower(ev.Title)] {
			continue
		}
		filtered = append(filtered, ev)
	}
	return filtered, nil
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
	log.Printf("[calendar-blog] Fetching events for next %d days (max %d posts)...", p.LookAheadDays, p.MaxPosts)

	now := time.Now().UTC()
	windowEnd := now.AddDate(0, 0, p.LookAheadDays)

	// Step 1: Fetch accurate festival dates from calendar-bharat API
	var events []upcomingEvent
	for _, year := range []int{now.Year(), now.Year() + 1} {
		yearEvents, err := fetchCalendarBharatEvents(year, now, windowEnd)
		if err != nil {
			log.Printf("[calendar-blog] WARN: calendar-bharat %d fetch failed: %v", year, err)
		} else {
			events = append(events, yearEvents...)
			log.Printf("[calendar-blog] calendar-bharat %d: %d events in window", year, len(yearEvents))
		}
	}

	existingTitles := map[string]bool{}
	for _, ev := range events {
		existingTitles[strings.ToLower(ev.Title)] = true
	}

	// Step 2: Prokerala — tithi-based astrological events (Ekadashi, Pradosh, Purnima, Amavasya)
	prokeralaEvents := s.fetchProkeralaEvents(now, windowEnd, existingTitles)
	log.Printf("[calendar-blog] Prokerala tithi events: %d", len(prokeralaEvents))
	for _, ev := range prokeralaEvents {
		existingTitles[strings.ToLower(ev.Title)] = true
	}
	events = append(events, prokeralaEvents...)

	// Step 3: OpenAI web search — retrogrades, eclipses, major transits only
	gapEvents, err := s.fetchOpenAIGapEvents(ctx, now, windowEnd, existingTitles)
	if err != nil {
		log.Printf("[calendar-blog] WARN: OpenAI gap events failed: %v", err)
	} else {
		log.Printf("[calendar-blog] OpenAI celestial gap events: %d", len(gapEvents))
		events = append(events, gapEvents...)
	}

	log.Printf("[calendar-blog] Total events to process: %d", len(events))

	if len(events) == 0 {
		log.Println("[calendar-blog] No upcoming events found — nothing to generate")
		return nil
	}

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
