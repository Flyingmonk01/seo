package workers

import (
	"context"
	"log"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
	"github.com/91astro/seo-agent/internal/models"
)

// aggregatePageStats computes page-level stats from raw data and stores in seo_page_stats.
// Called after ingest+analyze chain.
func (s *Server) aggregatePageStats(ctx context.Context, date string) error {
	log.Println("[aggregator] Computing page-level stats...")

	rawCol := s.db.Collection(models.ColRawData)
	statsCol := s.db.Collection(models.ColPageStats)

	// Aggregate raw data into page-level metrics
	pipeline := []bson.D{
		{{Key: "$match", Value: bson.D{{Key: "date", Value: date}}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$page"},
			{Key: "totalClicks", Value: bson.D{{Key: "$sum", Value: "$clicks"}}},
			{Key: "totalImpressions", Value: bson.D{{Key: "$sum", Value: "$impressions"}}},
			{Key: "avgCTR", Value: bson.D{{Key: "$avg", Value: "$ctr"}}},
			{Key: "avgPosition", Value: bson.D{{Key: "$avg", Value: "$position"}}},
			{Key: "queryCount", Value: bson.D{{Key: "$sum", Value: 1}}},
			// Top query by impressions
			{Key: "queries", Value: bson.D{{Key: "$push", Value: bson.D{
				{Key: "query", Value: "$query"},
				{Key: "impressions", Value: "$impressions"},
			}}}},
			// Device breakdown
			{Key: "mobileImpressions", Value: bson.D{{Key: "$sum", Value: bson.D{
				{Key: "$cond", Value: bson.A{
					bson.D{{Key: "$eq", Value: bson.A{"$device", "MOBILE"}}},
					"$impressions",
					0,
				}},
			}}}},
			// Country breakdown — most common
			{Key: "countries", Value: bson.D{{Key: "$push", Value: "$country"}}},
			// Intent breakdown — most common
			{Key: "intents", Value: bson.D{{Key: "$push", Value: "$intent"}}},
		}}},
	}

	cursor, err := rawCol.Aggregate(ctx, pipeline)
	if err != nil {
		return err
	}
	defer cursor.Close(ctx)

	var results []struct {
		Page              string  `bson:"_id"`
		TotalClicks       int64   `bson:"totalClicks"`
		TotalImpressions  int64   `bson:"totalImpressions"`
		AvgCTR            float64 `bson:"avgCTR"`
		AvgPosition       float64 `bson:"avgPosition"`
		QueryCount        int     `bson:"queryCount"`
		MobileImpressions int64   `bson:"mobileImpressions"`
		Queries           []struct {
			Query       string `bson:"query"`
			Impressions int64  `bson:"impressions"`
		} `bson:"queries"`
		Countries []string `bson:"countries"`
		Intents   []string `bson:"intents"`
	}
	if err := cursor.All(ctx, &results); err != nil {
		return err
	}

	// Fetch previous day stats for delta computation
	prevDate := previousDate(date)
	prevStats := map[string]*models.SeoPageStats{}
	prevCursor, _ := statsCol.Find(ctx, bson.D{{Key: "date", Value: prevDate}})
	if prevCursor != nil {
		var prev []models.SeoPageStats
		prevCursor.All(ctx, &prev)
		prevCursor.Close(ctx)
		for i := range prev {
			prevStats[prev[i].Page] = &prev[i]
		}
	}

	stored := 0
	for _, r := range results {
		// Find top query
		topQuery := ""
		topImpr := int64(0)
		for _, q := range r.Queries {
			if q.Impressions > topImpr {
				topImpr = q.Impressions
				topQuery = q.Query
			}
		}

		// Mobile share
		mobileShare := float64(0)
		if r.TotalImpressions > 0 {
			mobileShare = float64(r.MobileImpressions) / float64(r.TotalImpressions) * 100
		}

		// Top country
		topCountry := mostCommon(r.Countries)

		// Dominant intent
		dominantIntent := mostCommon(r.Intents)

		// Deltas vs previous period
		var clicksDelta int64
		var ctrDelta, posDelta float64
		if prev, ok := prevStats[r.Page]; ok {
			clicksDelta = r.TotalClicks - prev.TotalClicks
			ctrDelta = r.AvgCTR - prev.AvgCTR
			posDelta = r.AvgPosition - prev.AvgPosition
		}

		stat := models.SeoPageStats{
			Page:             r.Page,
			Date:             date,
			TotalClicks:      r.TotalClicks,
			TotalImpressions: r.TotalImpressions,
			AvgCTR:           r.AvgCTR,
			AvgPosition:      r.AvgPosition,
			TopQuery:         topQuery,
			QueryCount:       r.QueryCount,
			MobileShare:      mobileShare,
			TopCountry:       topCountry,
			DominantIntent:   dominantIntent,
			ClicksDelta:      clicksDelta,
			CTRDelta:         ctrDelta,
			PositionDelta:    posDelta,
			CreatedAt:        time.Now(),
		}

		// Upsert by page + date
		filter := bson.D{
			{Key: "page", Value: r.Page},
			{Key: "date", Value: date},
		}
		update := bson.D{{Key: "$set", Value: stat}}
		opts := options.Update().SetUpsert(true)
		statsCol.UpdateOne(ctx, filter, update, opts)
		stored++
	}

	log.Printf("[aggregator] Stored %d page stats for %s", stored, date)
	return nil
}

func mostCommon(items []string) string {
	if len(items) == 0 {
		return ""
	}
	counts := map[string]int{}
	for _, item := range items {
		if item != "" {
			counts[item]++
		}
	}
	best := ""
	bestCount := 0
	for k, v := range counts {
		if v > bestCount {
			best = k
			bestCount = v
		}
	}
	return best
}

func previousDate(date string) string {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return ""
	}
	return t.AddDate(0, 0, -1).Format("2006-01-02")
}
