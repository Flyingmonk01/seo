package workers

import (
	"context"
	"log"
	"sort"
	"time"

	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"github.com/91astro/seo-agent/internal/models"
	"github.com/91astro/seo-agent/internal/services"
)

func (s *Server) handleIngest(ctx context.Context, task *asynq.Task) error {
	log.Println("[ingest] ─────────────────────────────────────────")
	log.Println("[ingest] Fetching GSC data (4-day lag applied)...")

	rows, err := s.gsc.FetchDailyData(ctx)
	if err != nil {
		return err
	}

	log.Printf("[ingest] Received %d rows from GSC", len(rows))

	if len(rows) > 0 {
		// Show the date range we got
		dates := map[string]bool{}
		pageSet := map[string]bool{}
		var totalClicks, totalImpressions int64
		for _, r := range rows {
			dates[r.Date] = true
			pageSet[r.Page] = true
			totalClicks += r.Clicks
			totalImpressions += r.Impressions
		}
		log.Printf("[ingest] Date(s) in data: %v", sortedKeys(dates))
		log.Printf("[ingest] Unique pages:    %d", len(pageSet))
		log.Printf("[ingest] Unique queries:  %d", len(rows))
		log.Printf("[ingest] Total clicks:    %d", totalClicks)
		log.Printf("[ingest] Total impressions: %d", totalImpressions)

		// Top 5 pages by impressions
		type pageStat struct {
			page        string
			impressions int64
			clicks      int64
		}
		pageMap := map[string]*pageStat{}
		for _, r := range rows {
			if _, ok := pageMap[r.Page]; !ok {
				pageMap[r.Page] = &pageStat{page: r.Page}
			}
			pageMap[r.Page].impressions += r.Impressions
			pageMap[r.Page].clicks += r.Clicks
		}
		stats := make([]*pageStat, 0, len(pageMap))
		for _, v := range pageMap {
			stats = append(stats, v)
		}
		sort.Slice(stats, func(i, j int) bool {
			return stats[i].impressions > stats[j].impressions
		})
		log.Println("[ingest] Top 5 pages by impressions:")
		for i, s := range stats {
			if i >= 5 {
				break
			}
			ctr := float64(0)
			if s.impressions > 0 {
				ctr = float64(s.clicks) / float64(s.impressions) * 100
			}
			log.Printf("[ingest]   %d. %s — %d impr, %d clicks, %.2f%% CTR",
				i+1, s.page, s.impressions, s.clicks, ctr)
		}
	}

	col := s.db.Collection(models.ColRawData)

	// Ensure unique compound index so upserts work correctly
	col.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{
			{Key: "date", Value: 1},
			{Key: "page", Value: 1},
			{Key: "query", Value: 1},
			{Key: "device", Value: 1},
			{Key: "country", Value: 1},
		},
		Options: options.Index().SetUnique(true),
	})

	// Upsert each row — prevents duplicates if ingest reruns for the same date
	var ops []mongo.WriteModel
	now := time.Now()
	for _, r := range rows {
		filter := bson.D{
			{Key: "date", Value: r.Date},
			{Key: "page", Value: r.Page},
			{Key: "query", Value: r.Query},
			{Key: "device", Value: r.Device},
			{Key: "country", Value: r.Country},
		}
		update := bson.D{{Key: "$set", Value: models.SeoRawData{
			Page:        r.Page,
			Query:       r.Query,
			Clicks:      r.Clicks,
			Impressions: r.Impressions,
			CTR:         r.CTR,
			Position:    r.Position,
			Date:        r.Date,
			Device:      r.Device,
			Country:     r.Country,
			Intent:      string(services.ClassifyIntent(r.Query)),
			Cluster:     services.ClusterKey(r.Query),
			PosBucket:   string(services.GetPositionBucket(r.Position)),
			Locale:      "en",
			CreatedAt:   now,
		}}}
		ops = append(ops, mongo.NewUpdateOneModel().
			SetFilter(filter).
			SetUpdate(update).
			SetUpsert(true))
	}

	if len(ops) > 0 {
		res, err := col.BulkWrite(ctx, ops, options.BulkWrite().SetOrdered(false))
		if err != nil {
			log.Printf("[ingest] WARN: bulk upsert error: %v", err)
		} else {
			log.Printf("[ingest] Upserted %d rows (%d new, %d updated)",
				res.UpsertedCount+res.ModifiedCount, res.UpsertedCount, res.ModifiedCount)
		}
	}

	log.Printf("[ingest] Stored %d rows → chaining analyze...", len(rows))
	log.Println("[ingest] ─────────────────────────────────────────")

	return s.enqueue(TaskAnalyze, map[string]string{"date": time.Now().Format("2006-01-02")},
		asynq.Queue("default"),
		asynq.MaxRetry(3),
	)
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
