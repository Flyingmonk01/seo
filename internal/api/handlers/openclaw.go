package handlers

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"github.com/91astro/seo-agent/internal/models"
)

type OpenClawHandler struct {
	db *mongo.Database
}

func NewOpenClawHandler(db *mongo.Database) *OpenClawHandler {
	return &OpenClawHandler{db: db}
}

// GET /openclaw/tasks
// OpenClaw polls this to find pending code tasks.
// Returns the oldest pending task and marks it as in_progress.
func (h *OpenClawHandler) PollTask(c *gin.Context) {
	col := h.db.Collection(models.ColCodeTasks)

	now := time.Now()

	// Find oldest pending task and atomically mark it in_progress
	var task models.CodeTask
	err := col.FindOneAndUpdate(
		context.Background(),
		bson.D{{Key: "status", Value: models.CodeTaskPending}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: models.CodeTaskInProgress},
			{Key: "picked_up_at", Value: now},
		}}},
		options.FindOneAndUpdate().
			SetSort(bson.D{{Key: "created_at", Value: 1}}).
			SetReturnDocument(options.After),
	).Decode(&task)

	if err == mongo.ErrNoDocuments {
		c.JSON(http.StatusNoContent, nil) // nothing to do
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	log.Printf("[openclaw] Task picked up: id=%s title=%q files=%d",
		task.ID.Hex(), task.Title, len(task.Files))

	c.JSON(http.StatusOK, task)
}

// POST /openclaw/tasks/:id/result
// OpenClaw calls this after completing (or failing) a task.
// Body: { "prUrl": "https://...", "error": "" }
func (h *OpenClawHandler) ReportResult(c *gin.Context) {
	taskID, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid task id"})
		return
	}

	var body struct {
		PRUrl string `json:"prUrl"`
		Error string `json:"error"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	col := h.db.Collection(models.ColCodeTasks)
	featureCol := h.db.Collection(models.ColFeatures)

	var task models.CodeTask
	if err := col.FindOne(context.Background(), bson.D{{Key: "_id", Value: taskID}}).Decode(&task); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return
	}

	now := time.Now()

	if body.Error != "" {
		col.UpdateByID(context.Background(), taskID, bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: models.CodeTaskFailed},
			{Key: "error_msg", Value: body.Error},
			{Key: "completed_at", Value: now},
		}}})
		featureCol.UpdateByID(context.Background(), task.FeatureID, bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: models.FeatureCoding},
		}}})
		log.Printf("[openclaw] Task %s FAILED: %s", taskID.Hex(), body.Error)
		c.JSON(http.StatusOK, gin.H{"received": true})
		return
	}

	if body.PRUrl == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "prUrl required on success"})
		return
	}

	col.UpdateByID(context.Background(), taskID, bson.D{{Key: "$set", Value: bson.D{
		{Key: "status", Value: models.CodeTaskDone},
		{Key: "pr_url", Value: body.PRUrl},
		{Key: "completed_at", Value: now},
	}}})

	featureCol.UpdateByID(context.Background(), task.FeatureID, bson.D{{Key: "$set", Value: bson.D{
		{Key: "status", Value: models.FeaturePROpen},
		{Key: "pr_url", Value: body.PRUrl},
		{Key: "branch_name", Value: task.BranchName},
	}}})

	log.Printf("[openclaw] Task %s DONE — PR: %s", taskID.Hex(), body.PRUrl)
	c.JSON(http.StatusOK, gin.H{"received": true})
}

// GET /openclaw/tasks/pending/count
// Quick check — how many tasks are waiting.
func (h *OpenClawHandler) PendingCount(c *gin.Context) {
	col := h.db.Collection(models.ColCodeTasks)
	count, err := col.CountDocuments(context.Background(),
		bson.D{{Key: "status", Value: bson.D{{Key: "$in", Value: []models.CodeTaskStatus{
			models.CodeTaskPending,
			models.CodeTaskInProgress,
		}}}}},
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"pending": count})
}

// GET /openclaw/tasks/list
// Dashboard view — all code tasks with their current status.
func (h *OpenClawHandler) ListTasks(c *gin.Context) {
	col := h.db.Collection(models.ColCodeTasks)

	cursor, err := col.Find(context.Background(),
		bson.D{},
		options.Find().
			SetSort(bson.D{{Key: "created_at", Value: -1}}).
			SetLimit(50),
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var tasks []models.CodeTask
	cursor.All(context.Background(), &tasks)
	c.JSON(http.StatusOK, gin.H{"data": tasks, "count": len(tasks)})
}
