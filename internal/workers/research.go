package workers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"time"

	"github.com/91astro/seo-agent/internal/models"
	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

type researchPayload struct {
	TopN int `json:"top_n"`
}

var questionPattern = regexp.MustCompile(`(?i)^(how|what|why|when|which|can|is|does|who)\b`)

func (s *Server) handleResearch(ctx context.Context, task *asynq.Task) error {
	var p researchPayload
	if err := json.Unmarshal(task.Payload(), &p); err != nil || p.TopN == 0 {
		p.TopN = 5
	}

	log.Printf("[research] Researching top %d pages...", p.TopN)

	// Get top pages by impressions
	rawCol := s.db.Collection(models.ColRawData)
	pipeline := mongo.Pipeline{
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$page"},
			{Key: "totalImpressions", Value: bson.D{{Key: "$sum", Value: "$impressions"}}},
			{Key: "avgCTR", Value: bson.D{{Key: "$avg", Value: "$ctr"}}},
			{Key: "avgBounceRate", Value: bson.D{{Key: "$avg", Value: "$bounce_rate"}}},
			{Key: "queries", Value: bson.D{{Key: "$push", Value: "$query"}}},
		}}},
		{{Key: "$sort", Value: bson.D{{Key: "totalImpressions", Value: -1}}}},
		{{Key: "$limit", Value: int64(p.TopN)}},
	}

	cursor, err := rawCol.Aggregate(ctx, pipeline)
	if err != nil {
		return err
	}
	defer cursor.Close(ctx)

	var pages []struct {
		Page             string   `bson:"_id"`
		TotalImpressions int64    `bson:"totalImpressions"`
		AvgCTR           float64  `bson:"avgCTR"`
		AvgBounceRate    float64  `bson:"avgBounceRate"`
		Queries          []string `bson:"queries"`
	}
	cursor.All(ctx, &pages)

	featureCol := s.db.Collection(models.ColFeatures)
	researched := 0

	for _, page := range pages {
		// Skip pages already being worked on
		existing, _ := featureCol.CountDocuments(ctx, bson.D{
			{Key: "page", Value: page.Page},
			{Key: "status", Value: bson.D{{Key: "$in", Value: []models.FeatureStatus{
				models.FeatureResearching, models.FeaturePlanning,
				models.FeatureCoding, models.FeaturePROpen, models.FeatureMonitoring,
			}}}},
		})
		if existing > 0 {
			continue
		}

		signals := detectSignals(page.Page, page.Queries, page.AvgCTR, page.AvgBounceRate, page.TotalImpressions)
		if len(signals) == 0 {
			continue
		}

		// Enrich with real user behavior from GA + Datadog
		behavior, err := s.analytics.GetPageBehavior(ctx, page.Page, 30)
		if err != nil {
			log.Printf("WARN: GA behavior for %s: %v", page.Page, err)
		} else {
			// Add behavior-based signals
			if behavior.BounceRate > 70 && page.TotalImpressions > 1000 {
				signals = append(signals, models.ResearchSignal{
					Type:     "high_bounce",
					Evidence: formatf("%.0f%% bounce rate on %d impressions", behavior.BounceRate, page.TotalImpressions),
					Suggest:  "Content not matching intent — needs interactive element or better above-fold hook",
				})
			}
			if behavior.AvgSessionDuration > 120 && behavior.ConversionRate < 2 {
				signals = append(signals, models.ResearchSignal{
					Type:     "engaged_no_convert",
					Evidence: formatf("%.0fs avg session, %.1f%% conversion", behavior.AvgSessionDuration, behavior.ConversionRate),
					Suggest:  "Users are interested but not converting — add CTA or consultation widget",
				})
			}
		}

		rum, err := s.analytics.GetRUMData(ctx, page.Page)
		if err == nil && len(rum.RageClicks) > 0 {
			signals = append(signals, models.ResearchSignal{
				Type:     "rage_clicks",
				Evidence: formatf("Rage clicks on: %s", rum.RageClicks[0].Selector),
				Suggest:  "Users expect interactivity here — add functional widget",
			})
		}

		feature := models.SeoFeature{
			Page:      page.Page,
			Signals:   signals,
			Status:    models.FeatureResearching,
			CreatedAt: time.Now(),
		}

		result, err := featureCol.InsertOne(ctx, feature)
		if err != nil {
			continue
		}

		// Chain: plan this feature
		s.enqueue(TaskPlanFeature,
			map[string]interface{}{"feature_id": result.InsertedID},
			asynq.Queue("default"),
		)
		researched++
	}

	log.Printf("[research] Queued %d features for planning", researched)
	return nil
}

func detectSignals(page string, queries []string, avgCTR, bounceRate float64, impressions int64) []models.ResearchSignal {
	var signals []models.ResearchSignal

	// Count question-intent queries
	questionCount := 0
	for _, q := range queries {
		if questionPattern.MatchString(q) {
			questionCount++
		}
	}
	if questionCount >= 3 {
		signals = append(signals, models.ResearchSignal{
			Type:     "question_intent",
			Evidence: formatf("%d question-intent queries detected", questionCount),
			Suggest:  "Add FAQ widget — users are asking questions the page isn't answering",
		})
	}

	if impressions > 1000 && avgCTR < 2 {
		signals = append(signals, models.ResearchSignal{
			Type:     "low_ctr_high_volume",
			Evidence: formatf("%d impressions at %.1f%% CTR", impressions, avgCTR),
			Suggest:  "High visibility but low engagement — needs credibility or preview content improvement",
		})
	}

	return signals
}

func formatf(format string, args ...interface{}) string {
	return fmt.Sprintf(format, args...)
}
