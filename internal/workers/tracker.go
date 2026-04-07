package workers

import (
	"context"
	"log"
	"time"

	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/bson"
	"github.com/91astro/seo-agent/internal/models"
)

func (s *Server) handleTrackImpact(ctx context.Context, task *asynq.Task) error {
	log.Println("[tracker] Measuring impact of live changes...")

	changeCol := s.db.Collection(models.ColChanges)
	resultCol := s.db.Collection(models.ColResults)

	cursor, err := changeCol.Find(ctx, bson.D{})
	if err != nil {
		return err
	}
	defer cursor.Close(ctx)

	var changes []models.SeoChange
	if err := cursor.All(ctx, &changes); err != nil {
		return err
	}

	windows := []int{7, 14, 30}

	for _, change := range changes {
		for _, window := range windows {
			// Skip if this window was already measured
			existingCount, _ := resultCol.CountDocuments(ctx, bson.D{
				{Key: "change_id", Value: change.ID},
				{Key: "window", Value: window},
			})
			if existingCount > 0 {
				continue
			}

			// Only measure if enough time has passed
			if time.Since(change.AppliedAt) < time.Duration(window)*24*time.Hour {
				continue
			}

			startDate := change.AppliedAt.Format("2006-01-02")
			endDate := change.AppliedAt.AddDate(0, 0, window).Format("2006-01-02")

			after, err := s.gsc.FetchRangeData(ctx, startDate, endDate)
			if err != nil {
				log.Printf("WARN: GSC fetch for %s window %d: %v", change.Page, window, err)
				continue
			}

			// Aggregate after metrics for this page
			var afterMetrics models.MetricSnapshot
			var count int
			for _, row := range after {
				if row.Page == change.Page {
					afterMetrics.CTR += row.CTR
					afterMetrics.Position += row.Position
					afterMetrics.Clicks += row.Clicks
					afterMetrics.Impressions += row.Impressions
					count++
				}
			}
			if count > 0 {
				afterMetrics.CTR /= float64(count)
				afterMetrics.Position /= float64(count)
			}

			result := models.SeoResult{
				ChangeID: change.ID,
				Page:     change.Page,
				Window:   window,
				Before:   models.MetricSnapshot{CTR: change.BaselineMetrics.AvgCTR, Position: change.BaselineMetrics.AvgPosition},
				After:    afterMetrics,
				Delta: models.MetricDelta{
					CTRDelta:      afterMetrics.CTR - change.BaselineMetrics.AvgCTR,
					PositionDelta: afterMetrics.Position - change.BaselineMetrics.AvgPosition,
					ClicksDelta:   afterMetrics.Clicks - change.BaselineMetrics.TotalClicks,
				},
				MeasuredAt: time.Now(),
			}

			resultCol.InsertOne(ctx, result)
			log.Printf("[tracker] Measured %s window=%d CTR delta=%.2f%%", change.Page, window, result.Delta.CTRDelta)
		}
	}

	return nil
}
