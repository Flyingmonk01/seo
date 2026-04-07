package handlers

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"github.com/91astro/seo-agent/internal/models"
)

type StatsHandler struct {
	db *mongo.Database
}

func NewStatsHandler(db *mongo.Database) *StatsHandler {
	return &StatsHandler{db: db}
}

func (h *StatsHandler) GetStats(c *gin.Context) {
	ctx := context.Background()

	pendingMeta, _ := h.db.Collection(models.ColSuggestions).CountDocuments(ctx,
		bson.D{{Key: "status", Value: models.SuggestionPending}})
	liveMeta, _ := h.db.Collection(models.ColSuggestions).CountDocuments(ctx,
		bson.D{{Key: "status", Value: models.SuggestionLive}})
	openIssues, _ := h.db.Collection(models.ColIssues).CountDocuments(ctx,
		bson.D{{Key: "status", Value: models.IssuePendingGeneration}})
	featuresInProgress, _ := h.db.Collection(models.ColFeatures).CountDocuments(ctx,
		bson.D{{Key: "status", Value: bson.D{{Key: "$in", Value: []models.FeatureStatus{
			models.FeatureResearching, models.FeaturePlanning, models.FeatureCoding,
			models.FeaturePROpen, models.FeatureMonitoring,
		}}}}})
	featuresLive, _ := h.db.Collection(models.ColFeatures).CountDocuments(ctx,
		bson.D{{Key: "status", Value: models.FeatureLive}})
	featuresRolledBack, _ := h.db.Collection(models.ColFeatures).CountDocuments(ctx,
		bson.D{{Key: "status", Value: models.FeatureRolledBack}})

	// OpenClaw code task counts
	codeTaskPending, _ := h.db.Collection(models.ColCodeTasks).CountDocuments(ctx,
		bson.D{{Key: "status", Value: models.CodeTaskPending}})
	codeTaskInProgress, _ := h.db.Collection(models.ColCodeTasks).CountDocuments(ctx,
		bson.D{{Key: "status", Value: models.CodeTaskInProgress}})
	codeTaskDone, _ := h.db.Collection(models.ColCodeTasks).CountDocuments(ctx,
		bson.D{{Key: "status", Value: models.CodeTaskDone}})
	codeTaskFailed, _ := h.db.Collection(models.ColCodeTasks).CountDocuments(ctx,
		bson.D{{Key: "status", Value: models.CodeTaskFailed}})

	// Blog content pipeline stats
	blogPending, _ := h.db.Collection(models.ColBlogTopics).CountDocuments(ctx,
		bson.D{{Key: "status", Value: "pending"}})
	blogApproved, _ := h.db.Collection(models.ColBlogTopics).CountDocuments(ctx,
		bson.D{{Key: "status", Value: "approved"}})
	blogRejected, _ := h.db.Collection(models.ColBlogTopics).CountDocuments(ctx,
		bson.D{{Key: "status", Value: "rejected"}})
	blogRegenerating, _ := h.db.Collection(models.ColBlogTopics).CountDocuments(ctx,
		bson.D{{Key: "status", Value: "regenerating"}})

	c.JSON(http.StatusOK, gin.H{
		"metadata": gin.H{
			"pendingApproval": pendingMeta,
			"live":            liveMeta,
			"openIssues":      openIssues,
		},
		"features": gin.H{
			"inProgress": featuresInProgress,
			"live":       featuresLive,
			"rolledBack": featuresRolledBack,
		},
		"openclaw": gin.H{
			"pending":    codeTaskPending,
			"inProgress": codeTaskInProgress,
			"done":       codeTaskDone,
			"failed":     codeTaskFailed,
		},
		"blogContent": gin.H{
			"pending":      blogPending,
			"approved":     blogApproved,
			"rejected":     blogRejected,
			"regenerating": blogRegenerating,
		},
	})
}

func (h *StatsHandler) GetResults(c *gin.Context) {
	ctx := context.Background()

	cursor, err := h.db.Collection(models.ColResults).Find(ctx,
		bson.D{{Key: "window", Value: 7}},
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var results []models.SeoResult
	cursor.All(ctx, &results)
	c.JSON(http.StatusOK, gin.H{"data": results})
}

func (h *StatsHandler) GetLearnings(c *gin.Context) {
	ctx := context.Background()

	cursor, err := h.db.Collection(models.ColLearnings).Find(ctx, bson.D{})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var learnings []models.SeoLearning
	cursor.All(ctx, &learnings)
	c.JSON(http.StatusOK, gin.H{"data": learnings})
}
