package workers

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"github.com/91astro/seo-agent/internal/models"
)

type planFeaturePayload struct {
	FeatureID primitive.ObjectID `json:"feature_id"`
}

func (s *Server) handlePlanFeature(ctx context.Context, task *asynq.Task) error {
	var p planFeaturePayload
	if err := json.Unmarshal(task.Payload(), &p); err != nil {
		return err
	}

	featureCol := s.db.Collection(models.ColFeatures)
	learningCol := s.db.Collection(models.ColLearnings)

	var feature models.SeoFeature
	if err := featureCol.FindOne(ctx, bson.D{{Key: "_id", Value: p.FeatureID}}).Decode(&feature); err != nil {
		return err
	}

	// Fetch past successful learnings to inform the plan
	cursor, err := learningCol.Find(ctx, bson.D{{Key: "outcome", Value: models.OutcomeSuccess}})
	if err != nil {
		return err
	}
	var learnings []models.SeoLearning
	cursor.All(ctx, &learnings)
	cursor.Close(ctx)

	log.Printf("[planner] Planning feature for %s (%d signals, %d learnings)", feature.Page, len(feature.Signals), len(learnings))

	spec, err := s.openai.GenerateFeatureSpec(ctx, feature.Page, feature.Signals, learnings)
	if err != nil {
		return err
	}

	plan := &models.FeaturePlan{
		Title:      spec.Title,
		Hypothesis: spec.Hypothesis,
		Changes:    spec.Changes,
		SuccessMetrics: models.SuccessMetrics{
			BounceRateDelta: spec.SuccessMetrics.BounceRateDelta,
			SessionDelta:    spec.SuccessMetrics.SessionDelta,
			ConversionDelta: spec.SuccessMetrics.ConversionDelta,
		},
		RollbackTrigger: models.RollbackTrigger{
			BounceRateDelta: spec.RollbackTrigger.BounceRateDelta,
			ErrorRateDelta:  spec.RollbackTrigger.ErrorRateDelta,
		},
		FeatureFlagKey: spec.FeatureFlagKey,
	}

	featureCol.UpdateByID(ctx, feature.ID, bson.D{{Key: "$set", Value: bson.D{
		{Key: "plan", Value: plan},
		{Key: "status", Value: models.FeatureCoding},
	}}})

	// Chain: generate code
	return s.enqueue(TaskCodeFeature,
		map[string]interface{}{"feature_id": feature.ID},
		asynq.Queue("low"), // lower priority — heavy operation
		asynq.MaxRetry(2),
		asynq.Timeout(10*time.Minute),
	)
}
