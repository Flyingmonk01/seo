package workers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"github.com/91astro/seo-agent/internal/services"
)

type calendarBlogPayload struct {
	MaxPosts      int `json:"max_posts"`
	LookAheadDays int `json:"look_ahead_days"`
}

// calendarEvent represents a fixed astrological/festival event that drives SEO content.
type calendarEvent struct {
	Date        time.Time
	Title       string // short name, e.g. "Navratri 2026"
	Query       string // the SEO query to target
	Category    string // maps to CMS category
	Urgency     int    // days before event to publish (lead time)
}

// astroCalendar returns the hardcoded astrological event calendar for the current year.
// Add/update dates here each year. Events are sorted loosely by date.
func astroCalendar(year int) []calendarEvent {
	y := year
	return []calendarEvent{
		// ── Festivals ────────────────────────────────────────────────────────────
		{date(y, 1, 14), "Makar Sankranti " + itoa(y), "makar sankranti " + itoa(y) + " astrology significance", "Festival", 21},
		{date(y, 1, 26), "Vasant Panchami " + itoa(y), "vasant panchami " + itoa(y) + " puja vidhi muhurat", "Festival", 14},
		{date(y, 2, 26), "Mahashivratri " + itoa(y), "mahashivratri " + itoa(y) + " shiva puja astrology", "Festival", 21},
		{date(y, 3, 14), "Holi " + itoa(y), "holi " + itoa(y) + " astrology color significance", "Festival", 14},
		{date(y, 3, 30), "Chaitra Navratri " + itoa(y), "chaitra navratri " + itoa(y) + " dates muhurat", "Festival", 21},
		{date(y, 4, 6), "Ram Navami " + itoa(y), "ram navami " + itoa(y) + " puja timing significance", "Festival", 14},
		{date(y, 4, 10), "Hanuman Jayanti " + itoa(y), "hanuman jayanti " + itoa(y) + " kundli remedy", "Festival", 10},
		{date(y, 5, 12), "Buddha Purnima " + itoa(y), "buddha purnima " + itoa(y) + " full moon astrology", "Festival", 14},
		{date(y, 7, 7), "Guru Purnima " + itoa(y), "guru purnima " + itoa(y) + " jupiter significance vedic", "Festival", 14},
		{date(y, 8, 9), "Nag Panchami " + itoa(y), "nag panchami " + itoa(y) + " rahu ketu remedies", "Festival", 10},
		{date(y, 8, 16), "Raksha Bandhan " + itoa(y), "raksha bandhan " + itoa(y) + " muhurat astrology", "Festival", 14},
		{date(y, 8, 27), "Janmashtami " + itoa(y), "janmashtami " + itoa(y) + " krishna birth chart astrology", "Festival", 14},
		{date(y, 9, 22), "Pitru Paksha " + itoa(y), "pitru paksha " + itoa(y) + " shraddh dates significance", "Festival", 21},
		{date(y, 10, 2), "Shardiya Navratri " + itoa(y), "shardiya navratri " + itoa(y) + " dates ghatasthapana muhurat", "Festival", 21},
		{date(y, 10, 12), "Dussehra " + itoa(y), "dussehra " + itoa(y) + " vijayadashami astrology puja", "Festival", 14},
		{date(y, 10, 20), "Dhanteras " + itoa(y), "dhanteras " + itoa(y) + " shubh muhurat shopping astrology", "Festival", 10},
		{date(y, 10, 22), "Diwali " + itoa(y), "diwali " + itoa(y) + " lakshmi puja muhurat astrology", "Festival", 21},
		{date(y, 11, 5), "Chhath Puja " + itoa(y), "chhath puja " + itoa(y) + " significance surya astrology", "Festival", 14},
		{date(y, 12, 25), "Christmas Astrology " + itoa(y), "christmas " + itoa(y) + " astrology zodiac predictions", "Festival", 14},

		// ── Planetary Transits & Retrogrades ─────────────────────────────────────
		{date(y, 3, 29), "Saturn Retrograde " + itoa(y), "saturn retrograde " + itoa(y) + " effects zodiac signs", "Vedic", 21},
		{date(y, 5, 25), "Jupiter Retrograde " + itoa(y), "jupiter retrograde " + itoa(y) + " impact career finances", "Vedic", 21},
		{date(y, 7, 18), "Mercury Retrograde " + itoa(y), "mercury retrograde " + itoa(y) + " do's and don'ts", "Vedic", 14},
		{date(y, 9, 9), "Venus Retrograde " + itoa(y), "venus retrograde " + itoa(y) + " love relationships impact", "Vedic", 21},
		{date(y, 11, 9), "Mars Retrograde " + itoa(y), "mars retrograde " + itoa(y) + " energy career effects", "Vedic", 21},

		// ── Eclipses ─────────────────────────────────────────────────────────────
		{date(y, 3, 14), "Lunar Eclipse March " + itoa(y), "lunar eclipse march " + itoa(y) + " zodiac impact dos donts", "Vedic", 21},
		{date(y, 9, 7), "Lunar Eclipse September " + itoa(y), "lunar eclipse september " + itoa(y) + " chandra grahan effects", "Vedic", 21},
		{date(y, 3, 29), "Solar Eclipse March " + itoa(y), "solar eclipse march " + itoa(y) + " surya grahan impact rashi", "Vedic", 21},

		// ── New Year & Annual Predictions ────────────────────────────────────────
		{date(y, 1, 1), "New Year Predictions " + itoa(y), "astrology predictions " + itoa(y) + " all zodiac signs", "Zodiac", 14},
		{date(y, 4, 14), "Hindu New Year " + itoa(y), "hindu new year " + itoa(y) + " panchang predictions vedic", "Vedic", 14},

		// ── Seasonal / Monthly Horoscopes ────────────────────────────────────────
		{date(y, 1, 1), "January Horoscope " + itoa(y), "january " + itoa(y) + " monthly horoscope all signs", "Zodiac", 7},
		{date(y, 2, 1), "February Horoscope " + itoa(y), "february " + itoa(y) + " monthly horoscope predictions", "Zodiac", 7},
		{date(y, 3, 1), "March Horoscope " + itoa(y), "march " + itoa(y) + " monthly horoscope rashifal", "Zodiac", 7},
		{date(y, 4, 1), "April Horoscope " + itoa(y), "april " + itoa(y) + " monthly horoscope all zodiac", "Zodiac", 7},
		{date(y, 5, 1), "May Horoscope " + itoa(y), "may " + itoa(y) + " monthly horoscope vedic astrology", "Zodiac", 7},
		{date(y, 6, 1), "June Horoscope " + itoa(y), "june " + itoa(y) + " monthly horoscope career love", "Zodiac", 7},
		{date(y, 7, 1), "July Horoscope " + itoa(y), "july " + itoa(y) + " monthly horoscope rashifal", "Zodiac", 7},
		{date(y, 8, 1), "August Horoscope " + itoa(y), "august " + itoa(y) + " monthly horoscope predictions", "Zodiac", 7},
		{date(y, 9, 1), "September Horoscope " + itoa(y), "september " + itoa(y) + " monthly horoscope vedic", "Zodiac", 7},
		{date(y, 10, 1), "October Horoscope " + itoa(y), "october " + itoa(y) + " monthly horoscope all signs", "Zodiac", 7},
		{date(y, 11, 1), "November Horoscope " + itoa(y), "november " + itoa(y) + " monthly horoscope rashifal", "Zodiac", 7},
		{date(y, 12, 1), "December Horoscope " + itoa(y), "december " + itoa(y) + " monthly horoscope predictions", "Zodiac", 7},
	}
}

func date(year, month, day int) time.Time {
	return time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}

// handleCalendarBlogCreate generates blog posts for upcoming festivals and
// astrological events. It publishes content early enough for Google to index
// before the event (urgency = days before event to publish).
func (s *Server) handleCalendarBlogCreate(ctx context.Context, task *asynq.Task) error {
	var p calendarBlogPayload
	if err := json.Unmarshal(task.Payload(), &p); err != nil || p.MaxPosts == 0 {
		p.MaxPosts = 5
	}
	if p.LookAheadDays == 0 {
		p.LookAheadDays = 45
	}

	log.Println("[calendar-blog] ─────────────────────────────────────────")
	log.Printf("[calendar-blog] Looking for events in next %d days (max %d posts)...", p.LookAheadDays, p.MaxPosts)

	now := time.Now().UTC()
	windowEnd := now.AddDate(0, 0, p.LookAheadDays)

	// Build list of events we should publish for right now.
	// "Publish window": publish when (event.Date - urgency) <= now <= event.Date
	year := now.Year()
	events := append(astroCalendar(year), astroCalendar(year+1)...)

	var due []calendarEvent
	for _, ev := range events {
		publishFrom := ev.Date.AddDate(0, 0, -ev.Urgency)
		if now.Before(publishFrom) || now.After(ev.Date) {
			continue // not in publish window yet, or already past
		}
		if ev.Date.After(windowEnd) {
			continue // too far ahead
		}
		due = append(due, ev)
	}

	log.Printf("[calendar-blog] %d events due for content in the next %d days", len(due), p.LookAheadDays)

	if len(due) == 0 {
		log.Println("[calendar-blog] Nothing to generate — no upcoming events in window")
		return nil
	}

	// Dedup: skip events whose cluster key is already published
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
	for _, ev := range due {
		if created >= p.MaxPosts {
			break
		}

		clusterKey := services.ClusterKey(ev.Query)
		if usedClusterKeys[clusterKey] {
			log.Printf("[calendar-blog]   ~ skip %q — cluster key %q already used", ev.Title, clusterKey)
			continue
		}

		daysUntil := int(ev.Date.Sub(now).Hours() / 24)
		log.Printf("[calendar-blog] ── Event: %q (in %d days, query: %q) ──", ev.Title, daysUntil, ev.Query)

		customInstructions := fmt.Sprintf(
			"This article is for the upcoming event '%s' on %s (%d days away). "+
				"Include the specific date. Focus on: what to do, astrological significance, "+
				"Vedic remedies, dos and don'ts. Make it timely and actionable.",
			ev.Title, ev.Date.Format("2 January 2006"), daysUntil,
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
				log.Printf("[calendar-blog]   ! image generation failed (continuing without): %v", err)
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

	log.Printf("[calendar-blog] ── Summary: created %d event-based blog posts (hidden) ──", created)
	log.Println("[calendar-blog] ─────────────────────────────────────────")
	return nil
}
