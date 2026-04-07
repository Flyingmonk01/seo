package services

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	"google.golang.org/api/searchconsole/v1"
)

type GSCService struct {
	svc     *searchconsole.Service
	siteURL string
}

type GSCRow struct {
	Page        string
	Query       string
	Clicks      int64
	Impressions int64
	CTR         float64
	Position    float64
	Date        string
	Device      string // DESKTOP, MOBILE, TABLET
	Country     string // 3-letter country code
}

func NewGSCService(credentialsPath, siteURL string) (*GSCService, error) {
	ctx := context.Background()

	data, err := readFile(credentialsPath)
	if err != nil {
		return nil, fmt.Errorf("read GSC credentials: %w", err)
	}

	creds, err := google.CredentialsFromJSON(ctx, data,
		"https://www.googleapis.com/auth/webmasters.readonly",
	)
	if err != nil {
		return nil, fmt.Errorf("parse GSC credentials: %w", err)
	}

	svc, err := searchconsole.NewService(ctx, option.WithCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("create GSC service: %w", err)
	}

	return &GSCService{svc: svc, siteURL: siteURL}, nil
}

// FetchDailyData pulls data from 4 days ago with all dimensions.
func (g *GSCService) FetchDailyData(ctx context.Context) ([]GSCRow, error) {
	date := time.Now().AddDate(0, 0, -4).Format("2006-01-02")
	log.Printf("[gsc] Fetching data for %s (4-day lag)", date)
	return g.fetchRange(ctx, date, date)
}

// FetchRangeData pulls data for a custom date range (for impact tracking).
func (g *GSCService) FetchRangeData(ctx context.Context, startDate, endDate string) ([]GSCRow, error) {
	return g.fetchRange(ctx, startDate, endDate)
}

// FetchByDevice pulls data filtered by device type.
func (g *GSCService) FetchByDevice(ctx context.Context, startDate, endDate, device string) ([]GSCRow, error) {
	return g.fetchFiltered(ctx, startDate, endDate, "device", device)
}

// FetchByCountry pulls data filtered by country code.
func (g *GSCService) FetchByCountry(ctx context.Context, startDate, endDate, country string) ([]GSCRow, error) {
	return g.fetchFiltered(ctx, startDate, endDate, "country", country)
}

// FetchSearchAppearance pulls data with search appearance dimension
// to detect rich results, FAQ snippets, video results etc.
func (g *GSCService) FetchSearchAppearance(ctx context.Context, startDate, endDate string) ([]SearchAppearanceRow, error) {
	var rows []SearchAppearanceRow
	startRow := int64(0)
	rowLimit := int64(25000)

	for {
		req := &searchconsole.SearchAnalyticsQueryRequest{
			StartDate:  startDate,
			EndDate:    endDate,
			Dimensions: []string{"page", "searchAppearance"},
			RowLimit:   rowLimit,
			StartRow:   startRow,
		}

		resp, err := g.svc.Searchanalytics.Query(g.siteURL, req).Context(ctx).Do()
		if err != nil {
			return nil, fmt.Errorf("GSC search appearance query: %w", err)
		}

		for _, r := range resp.Rows {
			rows = append(rows, SearchAppearanceRow{
				Page:           r.Keys[0],
				Appearance:     r.Keys[1],
				Clicks:         int64(r.Clicks),
				Impressions:    int64(r.Impressions),
				CTR:            r.Ctr * 100,
				Position:       r.Position,
			})
		}

		if int64(len(resp.Rows)) < rowLimit {
			break
		}
		startRow += rowLimit
	}

	return rows, nil
}

type SearchAppearanceRow struct {
	Page        string
	Appearance  string // RICH_RESULT, FAQ, VIDEO, AMP, etc.
	Clicks      int64
	Impressions int64
	CTR         float64
	Position    float64
}

func (g *GSCService) fetchRange(ctx context.Context, startDate, endDate string) ([]GSCRow, error) {
	var rows []GSCRow
	startRow := int64(0)
	rowLimit := int64(25000)

	for {
		req := &searchconsole.SearchAnalyticsQueryRequest{
			StartDate:  startDate,
			EndDate:    endDate,
			Dimensions: []string{"page", "query", "device", "country", "date"},
			RowLimit:   rowLimit,
			StartRow:   startRow,
		}

		resp, err := g.svc.Searchanalytics.Query(g.siteURL, req).Context(ctx).Do()
		if err != nil {
			return nil, fmt.Errorf("GSC query failed: %w", err)
		}

		for _, r := range resp.Rows {
			rows = append(rows, GSCRow{
				Page:        r.Keys[0],
				Query:       r.Keys[1],
				Device:      r.Keys[2],
				Country:     r.Keys[3],
				Date:        r.Keys[4],
				Clicks:      int64(r.Clicks),
				Impressions: int64(r.Impressions),
				CTR:         r.Ctr * 100,
				Position:    r.Position,
			})
		}

		if int64(len(resp.Rows)) < rowLimit {
			break
		}
		startRow += rowLimit
	}

	return rows, nil
}

func (g *GSCService) fetchFiltered(ctx context.Context, startDate, endDate, dimension, value string) ([]GSCRow, error) {
	var rows []GSCRow
	startRow := int64(0)
	rowLimit := int64(25000)

	for {
		req := &searchconsole.SearchAnalyticsQueryRequest{
			StartDate:  startDate,
			EndDate:    endDate,
			Dimensions: []string{"page", "query"},
			RowLimit:   rowLimit,
			StartRow:   startRow,
			DimensionFilterGroups: []*searchconsole.ApiDimensionFilterGroup{
				{
					Filters: []*searchconsole.ApiDimensionFilter{
						{
							Dimension:  dimension,
							Operator:   "equals",
							Expression: value,
						},
					},
				},
			},
		}

		resp, err := g.svc.Searchanalytics.Query(g.siteURL, req).Context(ctx).Do()
		if err != nil {
			return nil, fmt.Errorf("GSC filtered query: %w", err)
		}

		for _, r := range resp.Rows {
			rows = append(rows, GSCRow{
				Page:        r.Keys[0],
				Query:       r.Keys[1],
				Device:      strings.ToUpper(value),
				Country:     "",
				Date:        startDate,
				Clicks:      int64(r.Clicks),
				Impressions: int64(r.Impressions),
				CTR:         r.Ctr * 100,
				Position:    r.Position,
			})
		}

		if int64(len(resp.Rows)) < rowLimit {
			break
		}
		startRow += rowLimit
	}

	return rows, nil
}

// ── Query Intelligence Helpers ────────────────────────────────────────────

// QueryIntent classifies a search query into intent type.
type QueryIntent string

const (
	IntentInformational QueryIntent = "informational" // how to, what is, guide
	IntentTransactional QueryIntent = "transactional" // buy, price, book, consult
	IntentNavigational  QueryIntent = "navigational"  // brand queries, specific page
	IntentLocal         QueryIntent = "local"          // near me, in city
)

// ClassifyIntent determines the search intent of a query.
func ClassifyIntent(query string) QueryIntent {
	q := strings.ToLower(query)

	// Transactional signals
	transactional := []string{"buy", "price", "cost", "book", "consult", "order",
		"purchase", "shop", "deal", "discount", "coupon", "free", "download",
		"subscribe", "premium", "paid", "hire"}
	for _, t := range transactional {
		if strings.Contains(q, t) {
			return IntentTransactional
		}
	}

	// Navigational signals (brand + specific page queries)
	navigational := []string{"91astro", "91astrology", "login", "signup", "app",
		"dashboard", "account", "profile"}
	for _, n := range navigational {
		if strings.Contains(q, n) {
			return IntentNavigational
		}
	}

	// Local signals
	local := []string{"near me", "in delhi", "in mumbai", "in bangalore",
		"in chennai", "in kolkata", "in hyderabad", "nearby", "local"}
	for _, l := range local {
		if strings.Contains(q, l) {
			return IntentLocal
		}
	}

	// Informational signals (most astrology queries are informational)
	informational := []string{"what", "how", "why", "when", "who", "which",
		"meaning", "effect", "impact", "prediction", "horoscope", "compatibility",
		"nakshatra", "rashi", "zodiac", "kundli", "transit", "retrograde",
		"remedy", "mantra", "yoga", "dasha", "mahadasha", "explain", "guide",
		"tips", "benefits"}
	for _, i := range informational {
		if strings.Contains(q, i) {
			return IntentInformational
		}
	}

	return IntentInformational // default for astrology domain
}

// PositionBucket returns a ranking tier for a position value.
type PositionBucket string

const (
	BucketTop3      PositionBucket = "top_3"       // position 1-3
	BucketFirstPage PositionBucket = "first_page"   // position 4-10
	BucketStriking  PositionBucket = "striking"      // position 11-20 (almost page 1)
	BucketDeep      PositionBucket = "deep"          // position 20+
)

func GetPositionBucket(position float64) PositionBucket {
	switch {
	case position <= 3:
		return BucketTop3
	case position <= 10:
		return BucketFirstPage
	case position <= 20:
		return BucketStriking
	default:
		return BucketDeep
	}
}

// ClusterKey normalizes a query for grouping similar queries.
// "kundli matching" and "kundli milan" → same cluster root.
func ClusterKey(query string) string {
	q := strings.ToLower(strings.TrimSpace(query))

	// Common Hindi/English synonyms in astrology
	replacements := map[string]string{
		"milan":        "matching",
		"kundali":      "kundli",
		"jathakam":     "kundli",
		"horoscop":     "horoscope",
		"rashifal":     "horoscope",
		"rasi":         "rashi",
		"prediction":   "horoscope",
		"daily":        "today",
		"today's":      "today",
		"todays":       "today",
		"weekly":       "week",
		"monthly":      "month",
		"yearly":       "year",
		"2025":         "",
		"2026":         "",
		"2027":         "",
		"free":         "",
		"online":       "",
		"best":         "",
	}

	for old, new := range replacements {
		q = strings.ReplaceAll(q, old, new)
	}

	// Remove extra spaces
	words := strings.Fields(q)
	if len(words) > 4 {
		words = words[:4] // keep first 4 words as cluster key
	}

	return strings.Join(words, " ")
}
