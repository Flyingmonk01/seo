package workers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/91astro/seo-agent/internal/services"
)

// ThemeEnrichment is the time-anchored research context produced for a
// trending GSC theme before the blog is planned and written.
//
// It is filled by a single call to OpenAI's web-search-capable model
// (gpt-4o-mini-search-preview) so the strategist/writer steps below
// can ground the post in current events instead of generic evergreen copy.
type ThemeEnrichment struct {
	CurrentMoment       string   `json:"currentMoment"`
	UpcomingHooks       []string `json:"upcomingHooks"`
	NewsAngles          []string `json:"newsAngles"`
	ContrarianTakes     []string `json:"contrarianTakes"`
	BestArchetype       string   `json:"bestArchetype"`
	RecommendedDateHook string   `json:"recommendedDateHook"`
	Reasoning           string   `json:"reasoning"`
}

const enrichmentWindowDays = 14

const enrichmentSystemPrompt = `You are a senior content strategist for an Indian Vedic astrology website (91astrology.com).
Your job: take a search-trending theme and find the most engaging, time-anchored angle for a blog post.

HARD RULES — VIOLATING THESE MEANS THE OUTPUT IS REJECTED:
1. Every date you mention MUST be on or after today and on or before today + 14 days. Past dates are forbidden.
2. If web search returns events from previous months, IGNORE them. Only return events whose date is within the allowed window.
3. The authoritative upcoming-events list provided in the user prompt is GROUND TRUTH. You may reference any event in that list. You may add news/celebrity angles via web search, but only with dates inside the allowed window.
4. Be specific with dates ("May 11, 2026") and named people. Vague references ("upcoming purnima") are a failure.
5. The angle MUST give the writer something a generic AI blog wouldn't have — a number, a named person, an urgency window, a contrarian claim, or a specific outcome.

Output ONLY valid JSON, no markdown fences, no commentary.`

// enrichTheme runs a single web-search-grounded research call for a trending
// GSC query and its sibling rising queries.
func (s *Server) enrichTheme(ctx context.Context, primary string, siblings []string) (*ThemeEnrichment, error) {
	now := time.Now()
	today := now.Format("2 January 2006")
	windowEndDate := now.AddDate(0, 0, enrichmentWindowDays)
	windowEnd := windowEndDate.Format("2 January 2006")

	siblingsBlock := ""
	if len(siblings) > 0 {
		siblingsBlock = "\nSibling rising queries (we want to rank for these too):\n- " + strings.Join(siblings, "\n- ")
	}

	authoritativeBlock := s.buildAuthoritativeUpcomingBlock(now, windowEndDate)

	user := fmt.Sprintf(`Today: %s
Allowed date window: %s through %s (inclusive). DO NOT reference any date outside this window.

Primary trending search query: %q
%s
%s

Find the most engaging angle for a blog post. Sources to use, in priority order:
1. The AUTHORITATIVE UPCOMING EVENTS list above (already verified to be in the window) — prefer these for any astrological/festival hook.
2. Web search for current news, celebrity events, or viral moments — but only events that occur on or after %s and on or before %s.
3. Contrarian takes / myths people believe about this topic.

The angle MUST:
- Anchor to a specific date inside the window OR a named current news event from inside the window.
- Have a viral hook: a number ("3 mistakes", "5 din"), a named person, an urgency window, a contrarian claim, or a specific outcome.
- Target the primary query and (if any) at least 2 sibling queries.

DO NOT include any date that is before %s. If you can't find a current event for the theme, anchor to one of the authoritative upcoming events above.

Output JSON:
{
  "currentMoment": "1-2 sentences on what's happening RIGHT NOW (between %s and %s) around this theme",
  "upcomingHooks": ["YYYY-MM-DD — event description (must be inside window)", "..."],
  "newsAngles": ["specific person/event with date inside window", "..."],
  "contrarianTakes": ["myth/misconception worth correcting", "..."],
  "bestArchetype": "one of: daily-horoscope | tool-explainer | celebrity-kundli | festival-guide | curiosity | transit-impact",
  "recommendedDateHook": "single exact date inside window — format YYYY-MM-DD plus event name",
  "reasoning": "why this angle is timely and will rank, 1-2 sentences"
}`,
		today,
		today, windowEnd,
		primary,
		siblingsBlock,
		authoritativeBlock,
		today, windowEnd,
		today,
		today, windowEnd,
	)

	resp, err := s.openai.Client().CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: "gpt-4o-mini-search-preview",
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: enrichmentSystemPrompt},
			{Role: openai.ChatMessageRoleUser, Content: user},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("enrichment call: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("enrichment returned no choices")
	}

	raw := cleanLLMJSON(resp.Choices[0].Message.Content)
	var out ThemeEnrichment
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("parse enrichment json: %w (raw: %s)", err, raw)
	}

	// Hard filter: drop any hook/angle whose embedded date isn't in the window.
	out.UpcomingHooks = filterDateStrings(out.UpcomingHooks, now, windowEndDate)
	out.NewsAngles = filterDateStrings(out.NewsAngles, now, windowEndDate)
	if !dateStringInWindow(out.RecommendedDateHook, now, windowEndDate) {
		// Fall back to the first valid upcoming hook, else clear it.
		out.RecommendedDateHook = ""
		if len(out.UpcomingHooks) > 0 {
			out.RecommendedDateHook = out.UpcomingHooks[0]
		}
	}

	return &out, nil
}

// buildAuthoritativeUpcomingBlock returns a list of REAL upcoming astrological
// events in the [from, to] window, computed locally so the model cannot
// hallucinate dates. Currently sourced from prokerala (tithis like Ekadashi,
// Purnima, Amavasya, Pradosh). Returns empty string if prokerala is not
// configured — in that case the model must fall back to web search.
func (s *Server) buildAuthoritativeUpcomingBlock(from, to time.Time) string {
	if s.prokerala == nil || !s.prokerala.IsConfigured() {
		return ""
	}
	anchor, err := s.prokerala.FetchTodayTithi(from)
	if err != nil {
		log.Printf("[enrichment] prokerala anchor fetch failed: %v", err)
		return ""
	}
	events := services.ComputeSignificantDates(anchor, from, to)
	if len(events) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("\nAUTHORITATIVE UPCOMING EVENTS (verified real, computed from today's panchang — use these as the source of truth):\n")
	for _, e := range events {
		sb.WriteString(fmt.Sprintf("- %s — %s\n", e.Date.Format("2006-01-02"), e.Name))
	}
	return sb.String()
}

// renderEnrichmentBlock turns the enrichment into a prompt fragment that the
// strategist must consult when picking the angle, heading, and sections.
func renderEnrichmentBlock(e *ThemeEnrichment) string {
	if e == nil {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\nCURRENT-MOMENT RESEARCH (use this to make the post timely — anchor the heading and at least one section to a specific event/date below; ALL dates listed here are guaranteed to be in the next 14 days):\n")
	if e.CurrentMoment != "" {
		sb.WriteString(fmt.Sprintf("- What's happening now: %s\n", e.CurrentMoment))
	}
	if len(e.UpcomingHooks) > 0 {
		sb.WriteString("- Upcoming hooks (pick the strongest, in window):\n")
		for _, h := range e.UpcomingHooks {
			sb.WriteString(fmt.Sprintf("  • %s\n", h))
		}
	}
	if len(e.NewsAngles) > 0 {
		sb.WriteString("- News/celebrity angles:\n")
		for _, n := range e.NewsAngles {
			sb.WriteString(fmt.Sprintf("  • %s\n", n))
		}
	}
	if len(e.ContrarianTakes) > 0 {
		sb.WriteString("- Contrarian takes / myths to bust:\n")
		for _, c := range e.ContrarianTakes {
			sb.WriteString(fmt.Sprintf("  • %s\n", c))
		}
	}
	if e.BestArchetype != "" {
		sb.WriteString(fmt.Sprintf("- Recommended archetype: %s\n", e.BestArchetype))
	}
	if e.RecommendedDateHook != "" {
		sb.WriteString(fmt.Sprintf("- Anchor date: %s\n", e.RecommendedDateHook))
	}
	if e.Reasoning != "" {
		sb.WriteString(fmt.Sprintf("- Why this angle: %s\n", e.Reasoning))
	}
	sb.WriteString(`
ARCHETYPE → STRUCTURE GUIDANCE:
- daily-horoscope: this-week format, focus on 1-2 transits + practical do/don't per day
- tool-explainer: first-person framing of using the tool, plus what the chart factor actually means
- celebrity-kundli: tie to a current event in the person's life; analyze 2-3 chart factors that explain it
- festival-guide: vidhi (method), exact muhurat/timing, do/don't, common mistakes
- curiosity: open with the question, give Vedic-shastra grounding, then a 5-point checklist
- transit-impact: who's affected most, exact dates, specific remedies per nakshatra/rashi
`)
	return sb.String()
}

// ── Date validation helpers ─────────────────────────────────────────────────

// Matches YYYY-MM-DD or "Month D, YYYY" patterns.
var (
	dateISORE   = regexp.MustCompile(`(\d{4})-(\d{2})-(\d{2})`)
	dateLongRE  = regexp.MustCompile(`(?i)(January|February|March|April|May|June|July|August|September|October|November|December)\s+(\d{1,2}),?\s+(\d{4})`)
	dateShortRE = regexp.MustCompile(`(?i)(\d{1,2})\s+(January|February|March|April|May|June|July|August|September|October|November|December)\s+(\d{4})`)
)

func parseDateFromString(s string) (time.Time, bool) {
	if m := dateISORE.FindStringSubmatch(s); m != nil {
		if t, err := time.Parse("2006-01-02", m[0]); err == nil {
			return t, true
		}
	}
	if m := dateLongRE.FindStringSubmatch(s); m != nil {
		layouts := []string{"January 2, 2006", "January 2 2006"}
		v := fmt.Sprintf("%s %s, %s", m[1], m[2], m[3])
		for _, layout := range layouts {
			if t, err := time.Parse(layout, v); err == nil {
				return t, true
			}
		}
	}
	if m := dateShortRE.FindStringSubmatch(s); m != nil {
		v := fmt.Sprintf("%s %s %s", m[1], m[2], m[3])
		if t, err := time.Parse("2 January 2006", v); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// dateStringInWindow returns true if the string contains no date at all
// (string can't be invalidated) or contains a date within [from, to].
// Strings with a date OUTSIDE the window return false.
func dateStringInWindow(s string, from, to time.Time) bool {
	if s == "" {
		return false
	}
	t, ok := parseDateFromString(s)
	if !ok {
		return true // no parseable date — let the strategist sanity-check
	}
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	fromDay := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	toDay := time.Date(to.Year(), to.Month(), to.Day(), 23, 59, 59, 0, time.UTC)
	return !day.Before(fromDay) && !day.After(toDay)
}

func filterDateStrings(in []string, from, to time.Time) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if dateStringInWindow(s, from, to) {
			out = append(out, s)
		}
	}
	return out
}
