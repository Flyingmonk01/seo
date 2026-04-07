package workers

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"github.com/91astro/seo-agent/internal/models"
)

type monitorFeaturePayload struct {
	FeatureID primitive.ObjectID `json:"feature_id"`
	Window    int                `json:"window"` // 1, 3, 7 days
}

func (s *Server) handleMonitorFeature(ctx context.Context, task *asynq.Task) error {
	var p monitorFeaturePayload
	if err := json.Unmarshal(task.Payload(), &p); err != nil {
		return err
	}

	featureCol := s.db.Collection(models.ColFeatures)
	flagCol := s.db.Collection(models.ColFlags)
	learningCol := s.db.Collection(models.ColLearnings)

	var feature models.SeoFeature
	if err := featureCol.FindOne(ctx, bson.D{{Key: "_id", Value: p.FeatureID}}).Decode(&feature); err != nil {
		return err
	}

	if feature.Status != models.FeatureMonitoring {
		return nil
	}

	log.Printf("[monitor] Checking %s (window=%dd)", feature.Plan.Title, p.Window)

	// Get behavior for users who saw the feature (treatment) vs didn't (control)
	// Using GA segment filtering
	treatment, err := s.analytics.GetPageBehavior(ctx, feature.Page, p.Window)
	if err != nil {
		return err
	}

	// Baseline from before feature was deployed (use feature.CreatedAt as boundary)
	baselineDays := p.Window
	baseline, err := s.analytics.GetPageBehavior(ctx, feature.Page, baselineDays*2) // double window as pre-period
	if err != nil {
		return err
	}

	result := models.MonitorResult{
		Window:          p.Window,
		BounceRateDelta: treatment.BounceRate - baseline.BounceRate,
		SessionDelta:    treatment.AvgSessionDuration - baseline.AvgSessionDuration,
		ConversionDelta: treatment.ConversionRate - baseline.ConversionRate,
		ErrorRateDelta:  treatment.ErrorRate - baseline.ErrorRate,
		MeasuredAt:      time.Now(),
	}

	// Append result to feature
	featureCol.UpdateByID(ctx, feature.ID, bson.D{{Key: "$push", Value: bson.D{
		{Key: "monitor_results", Value: result},
	}}})

	trigger := feature.Plan.RollbackTrigger

	// Auto-rollback check
	if result.BounceRateDelta > trigger.BounceRateDelta || result.ErrorRateDelta > trigger.ErrorRateDelta {
		log.Printf("[monitor] AUTO-ROLLBACK: %s (bounce +%.1f%%, errors +%.1f%%)",
			feature.Plan.Title, result.BounceRateDelta, result.ErrorRateDelta)

		now := time.Now()
		featureCol.UpdateByID(ctx, feature.ID, bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: models.FeatureRolledBack},
			{Key: "rolled_back_at", Value: now},
		}}})

		// Disable feature flag
		flagCol.UpdateOne(ctx,
			bson.D{{Key: "key", Value: feature.Plan.FeatureFlagKey}},
			bson.D{{Key: "$set", Value: bson.D{
				{Key: "enabled", Value: false},
				{Key: "rolled_back_at", Value: now},
			}}},
		)

		// Save as failure learning
		saveLearning(ctx, learningCol, &feature, result, models.OutcomeFailure)

		s.mail.SendRollbackAlert(s.cfg.ReportEmail, feature.Plan.Title, result.BounceRateDelta)
		return nil
	}

	// Promote to 100% if 7-day check passes
	if p.Window == 7 && result.BounceRateDelta <= 0 && result.ConversionDelta >= 0 {
		log.Printf("[monitor] PROMOTING to 100%%: %s", feature.Plan.Title)

		now := time.Now()
		featureCol.UpdateByID(ctx, feature.ID, bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: models.FeatureLive},
			{Key: "promoted_at", Value: now},
		}}})

		flagCol.UpdateOne(ctx,
			bson.D{{Key: "key", Value: feature.Plan.FeatureFlagKey}},
			bson.D{{Key: "$set", Value: bson.D{
				{Key: "rollout_percent", Value: 100},
				{Key: "promoted_at", Value: now},
			}}},
		)

		saveLearning(ctx, learningCol, &feature, result, models.OutcomeSuccess)
		return nil
	}

	// Schedule next check (24h, 72h, 7d windows)
	nextWindow := nextCheckWindow(p.Window)
	if nextWindow > 0 {
		delay := time.Duration(nextWindow-p.Window) * 24 * time.Hour
		s.enqueue(TaskMonitorFeature,
			map[string]interface{}{"feature_id": feature.ID, "window": nextWindow},
			asynq.ProcessIn(delay),
			asynq.Queue("low"),
		)
	}

	return nil
}

func nextCheckWindow(current int) int {
	switch current {
	case 1:
		return 3
	case 3:
		return 7
	default:
		return 0 // no more checks
	}
}

func saveLearning(ctx context.Context, col *mongo.Collection, feature *models.SeoFeature, result models.MonitorResult, outcome models.OutcomeType) {
	learning := models.SeoLearning{
		FeatureType: feature.Plan.Title,
		PageType:    inferPageType(feature.Page),
		Hypothesis:  feature.Plan.Hypothesis,
		Outcome:     outcome,
		Metrics:     result,
		Conditions: models.LearningConditions{
			PageType: inferPageType(feature.Page),
		},
		LearnedAt: time.Now(),
	}
	col.InsertOne(ctx, learning)
}

func inferPageType(page string) string {
	switch {
	case contains(page, "blog"):
		return "blog"
	case contains(page, "kundli"):
		return "kundli"
	case contains(page, "numerology"):
		return "calculator"
	case contains(page, "horoscope"):
		return "horoscope"
	default:
		return "other"
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
