package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
	openai "github.com/sashabaranov/go-openai"
)

// Theme bundles a primary GSC trending query with sibling queries we want to
// rank for in the same post.
type Theme struct {
	Primary  string
	Siblings []string
	Lang     string
}

// EnrichmentResult is what we want the search model to produce per theme.
type EnrichmentResult struct {
	CurrentMoment   string   `json:"currentMoment"`     // 1-2 sentences: what's happening right now around this theme
	UpcomingHooks   []string `json:"upcomingHooks"`      // dates/events in next 14d that tie to this theme
	NewsAngles      []string `json:"newsAngles"`         // current news/celebrity/cultural angles
	ContrarianTakes []string `json:"contrarianTakes"`    // myths/misconceptions/strong opinions worth taking
	BestArchetype   string   `json:"bestArchetype"`      // daily-horoscope|tool-explainer|celebrity-kundli|festival-guide|curiosity|transit-impact
	RecommendedDateHook string `json:"recommendedDateHook"` // exact date or window the post should anchor to
	Reasoning       string   `json:"reasoning"`           // why this angle, 1-2 sentences
}

const enrichSystem = `You are a senior content strategist for an Indian Vedic astrology website (91astrology.com).
Your job: take a search-trending theme and find the most engaging, time-anchored angle for a blog post.
Use web search to find what is happening RIGHT NOW in India around this theme — festivals, planetary transits, celebrity news, viral moments, current events.
Your output decides whether the blog feels timely or generic. Be specific with dates and names.
Output ONLY valid JSON, no markdown fences, no commentary.`

func enrichTheme(ctx context.Context, client *openai.Client, theme Theme, today string) (*EnrichmentResult, string, error) {
	siblingsBlock := ""
	if len(theme.Siblings) > 0 {
		siblingsBlock = "\nSibling rising queries (we want to rank for these too):\n- " + strings.Join(theme.Siblings, "\n- ")
	}

	user := fmt.Sprintf(`Today: %s
Primary trending search query: %q
Language/audience: %s
%s

Research what's happening in India RIGHT NOW that connects to this theme. Look for:
1. Planetary transits, retrogrades, eclipses, or major astrological events in the next 14 days
2. Festivals, vrats, ekadashis, purnima/amavasya in the next 14 days
3. Celebrity news, Bollywood, cricket, politics — any famous person whose chart/kundli ties to this theme
4. Viral moments, memes, controversies, news events
5. Common misconceptions or myths people believe about this topic

Then pick ONE specific angle that:
- Is anchored to a date in the next 14 days OR a current news event
- Targets the primary query AND at least 2 sibling queries
- Has a strong hook (number, contrarian claim, specific person, exact date)

Output JSON:
{
  "currentMoment": "1-2 sentences on what's happening right now around this theme",
  "upcomingHooks": ["date + event 1", "date + event 2"],
  "newsAngles": ["specific person/event/news angle 1", "..."],
  "contrarianTakes": ["myth/misconception 1 worth correcting", "..."],
  "bestArchetype": "one of: daily-horoscope | tool-explainer | celebrity-kundli | festival-guide | curiosity | transit-impact",
  "recommendedDateHook": "exact date or window in next 14d the post should anchor to",
  "reasoning": "why this angle will feel timely and rank, 1-2 sentences"
}`, today, theme.Primary, theme.Lang, siblingsBlock)

	resp, err := client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: "gpt-4o-mini-search-preview",
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: enrichSystem},
			{Role: openai.ChatMessageRoleUser, Content: user},
		},
	})
	if err != nil {
		return nil, "", err
	}

	raw := resp.Choices[0].Message.Content
	clean := strings.TrimSpace(raw)
	if strings.HasPrefix(clean, "```") {
		if idx := strings.Index(clean, "\n"); idx != -1 {
			clean = clean[idx+1:]
		}
		if idx := strings.LastIndex(clean, "```"); idx != -1 {
			clean = clean[:idx]
		}
		clean = strings.TrimSpace(clean)
	}

	var out EnrichmentResult
	if err := json.Unmarshal([]byte(clean), &out); err != nil {
		return nil, raw, fmt.Errorf("parse: %w", err)
	}
	return &out, raw, nil
}

func main() {
	_ = godotenv.Load()
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		log.Fatal("OPENAI_API_KEY missing")
	}
	client := openai.NewClient(apiKey)
	today := time.Now().Format("2 January 2006")

	themes := []Theme{
		{
			Primary: "মকর রাশি আজকের রাশিফল",
			Siblings: []string{
				"আজকের মকর রাশি",
				"মকর রাশিফল",
				"মকর রাশির রাশিফল",
				"মকর রাশি today",
			},
			Lang: "Bengali (Indian Bengali speakers searching daily horoscope)",
		},
		{
			Primary: "love or arrange marriage calculator",
			Siblings: []string{
				"love marriage and arrange marriage calculator",
				"love or arranged marriage calculator",
				"shadi kab hogi kaise pata kare online",
			},
			Lang: "English / Hinglish (Indian Gen-Z + millennial)",
		},
		{
			Primary:  "harbhajan singh kundli",
			Siblings: []string{},
			Lang:     "English (Indian cricket fans, astrology readers)",
		},
	}

	ctx := context.Background()
	for i, th := range themes {
		fmt.Printf("\n══════════════════════════════════════════════════════════════════════════\n")
		fmt.Printf("THEME %d: %s\n", i+1, th.Primary)
		fmt.Printf("══════════════════════════════════════════════════════════════════════════\n")
		res, raw, err := enrichTheme(ctx, client, th, today)
		if err != nil {
			fmt.Printf("ERROR: %v\nraw response:\n%s\n", err, raw)
			continue
		}
		out, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(out))
	}
}
