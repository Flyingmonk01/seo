package workers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"github.com/91astro/seo-agent/internal/models"
)

type codeFeaturePayload struct {
	FeatureID primitive.ObjectID `json:"feature_id"`
}

func (s *Server) handleCodeFeature(ctx context.Context, task *asynq.Task) error {
	var p codeFeaturePayload
	if err := json.Unmarshal(task.Payload(), &p); err != nil {
		return err
	}

	featureCol := s.db.Collection(models.ColFeatures)
	taskCol := s.db.Collection(models.ColCodeTasks)

	var feature models.SeoFeature
	if err := featureCol.FindOne(ctx, bson.D{{Key: "_id", Value: p.FeatureID}}).Decode(&feature); err != nil {
		return err
	}

	if feature.Plan == nil {
		return fmt.Errorf("feature %s has no plan", feature.ID.Hex())
	}

	log.Println("[coder] ─────────────────────────────────────────")
	log.Printf("[coder] Building code task for: %s", feature.Plan.Title)
	log.Printf("[coder] Hypothesis: %s", feature.Plan.Hypothesis)
	log.Printf("[coder] Files to change: %d", len(feature.Plan.Changes))

	branchName := fmt.Sprintf("seo-agent/%s-%d",
		slugify(feature.Plan.Title),
		time.Now().Unix(),
	)

	// Build file list for OpenClaw
	var files []models.CodeTaskFile
	for _, change := range feature.Plan.Changes {
		repo := "website"
		if isAPIFile(change.File) {
			repo = "api"
		}
		files = append(files, models.CodeTaskFile{
			Repo:        repo,
			FilePath:    change.File,
			Description: change.Description,
		})
		log.Printf("[coder]   [%s] %s — %s", repo, change.File, change.Description)
	}

	if len(files) == 0 {
		return fmt.Errorf("no file changes planned for feature %s", feature.Plan.Title)
	}

	// Queue the task for OpenClaw to pick up
	codeTask := models.CodeTask{
		FeatureID:  feature.ID,
		Title:      feature.Plan.Title,
		Hypothesis: feature.Plan.Hypothesis,
		BranchName: branchName,
		Files:      files,
		Status:     models.CodeTaskPending,
		CreatedAt:  time.Now(),
	}

	result, err := taskCol.InsertOne(ctx, codeTask)
	if err != nil {
		return fmt.Errorf("insert code task: %w", err)
	}

	taskID := result.InsertedID.(primitive.ObjectID).Hex()
	log.Printf("[coder] ✓ Code task queued (id=%s) — waiting for OpenClaw to pick up", taskID)
	log.Printf("[coder]   OpenClaw polls: GET /openclaw/tasks")
	log.Printf("[coder]   OpenClaw reports back: POST /openclaw/tasks/%s/result", taskID)

	// Update feature status to coding/waiting
	featureCol.UpdateByID(ctx, feature.ID, bson.D{{Key: "$set", Value: bson.D{
		{Key: "status", Value: models.FeatureCoding},
	}}})

	log.Println("[coder] ─────────────────────────────────────────")
	return nil
}

// handleCodeTaskResult is called by OpenClaw after it completes a task.
// This is invoked from the API handler, not Asynq.
func (s *Server) CompleteCodeTask(ctx context.Context, taskID primitive.ObjectID, prURL string, errMsg string) error {
	taskCol := s.db.Collection(models.ColCodeTasks)
	featureCol := s.db.Collection(models.ColFeatures)

	var codeTask models.CodeTask
	if err := taskCol.FindOne(ctx, bson.D{{Key: "_id", Value: taskID}}).Decode(&codeTask); err != nil {
		return fmt.Errorf("code task not found: %w", err)
	}

	now := time.Now()

	if errMsg != "" {
		// OpenClaw reported failure
		taskCol.UpdateByID(ctx, taskID, bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: models.CodeTaskFailed},
			{Key: "error_msg", Value: errMsg},
			{Key: "completed_at", Value: now},
		}}})
		featureCol.UpdateByID(ctx, codeTask.FeatureID, bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: models.FeatureCoding}, // stays in coding, can retry
		}}})
		log.Printf("[coder] OpenClaw reported failure for task %s: %s", taskID.Hex(), errMsg)
		return nil
	}

	// Success — update task + feature with PR info
	taskCol.UpdateByID(ctx, taskID, bson.D{{Key: "$set", Value: bson.D{
		{Key: "status", Value: models.CodeTaskDone},
		{Key: "pr_url", Value: prURL},
		{Key: "completed_at", Value: now},
	}}})

	featureCol.UpdateByID(ctx, codeTask.FeatureID, bson.D{{Key: "$set", Value: bson.D{
		{Key: "status", Value: models.FeaturePROpen},
		{Key: "pr_url", Value: prURL},
		{Key: "branch_name", Value: codeTask.BranchName},
	}}})

	log.Printf("[coder] ✓ OpenClaw completed task %s — PR: %s", taskID.Hex(), prURL)

	// Chain: start monitoring after a 7-day delay
	return s.enqueue(TaskMonitorFeature,
		map[string]interface{}{"feature_id": codeTask.FeatureID},
		asynq.Queue("low"),
		asynq.ProcessIn(7*24*time.Hour),
	)
}

func isAPIFile(filePath string) bool {
	return strings.HasPrefix(filePath, "src/modules/") ||
		strings.HasPrefix(filePath, "src/app.module")
}

func slugify(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " ", "-")
	s = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return -1
	}, s)
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}
