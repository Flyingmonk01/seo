package workers

import (
	"context"
	"log"

	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/bson"
	"github.com/91astro/seo-agent/internal/models"
	"github.com/91astro/seo-agent/internal/services"
)

func (s *Server) handleReport(ctx context.Context, task *asynq.Task) error {
	log.Println("[reporter] Generating weekly report...")

	resultCol := s.db.Collection(models.ColResults)
	suggCol := s.db.Collection(models.ColSuggestions)

	// Pages optimized this week
	liveCount, _ := suggCol.CountDocuments(ctx, bson.D{{Key: "status", Value: models.SuggestionLive}})
	pendingCount, _ := suggCol.CountDocuments(ctx, bson.D{{Key: "status", Value: models.SuggestionPending}})

	// Aggregate CTR deltas from 7-day results
	cursor, err := resultCol.Find(ctx, bson.D{{Key: "window", Value: 7}})
	if err != nil {
		return err
	}
	defer cursor.Close(ctx)

	var results []models.SeoResult
	cursor.All(ctx, &results)

	var totalBefore, totalAfter float64
	var keywordsImproved int
	var topWins []services.ReportWin

	for _, r := range results {
		totalBefore += r.Before.CTR
		totalAfter += r.After.CTR
		if r.Delta.PositionDelta < -3 {
			keywordsImproved++
		}
		if r.Delta.CTRDelta > 0.5 {
			topWins = append(topWins, services.ReportWin{
				Page:      r.Page,
				CTRBefore: r.Before.CTR,
				CTRAfter:  r.After.CTR,
			})
		}
	}

	n := float64(len(results))
	avgBefore, avgAfter := 0.0, 0.0
	if n > 0 {
		avgBefore = totalBefore / n
		avgAfter = totalAfter / n
	}

	report := &services.WeeklyReport{
		PagesOptimized:   int(liveCount),
		AvgCTRBefore:     avgBefore,
		AvgCTRAfter:      avgAfter,
		KeywordsTop5:     keywordsImproved,
		TopWins:          topWins,
		PendingApprovals: int(pendingCount),
	}

	if err := s.mail.SendWeeklyReport(s.cfg.ReportEmail, report); err != nil {
		log.Printf("WARN: send report email: %v", err)
	}

	log.Printf("[reporter] Report sent. Pages: %d, Avg CTR: %.2f → %.2f", int(liveCount), avgBefore, avgAfter)
	return nil
}
