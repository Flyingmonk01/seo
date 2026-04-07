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
	"github.com/91astro/seo-agent/internal/services"
)

type QueueHandler struct {
	db      *mongo.Database
	execute *services.ExecuteService
}

func NewQueueHandler(db *mongo.Database, execute *services.ExecuteService) *QueueHandler {
	return &QueueHandler{db: db, execute: execute}
}

// GET /queue — pending meta suggestions
func (h *QueueHandler) ListQueue(c *gin.Context) {
	col := h.db.Collection(models.ColSuggestions)

	cursor, err := col.Find(context.Background(),
		bson.D{{Key: "status", Value: bson.D{{Key: "$in", Value: []models.SuggestionStatus{
			models.SuggestionPending,
			models.SuggestionApproved,
			models.SuggestionLive,
		}}}}},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(100),
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var suggestions []models.SeoSuggestion
	cursor.All(context.Background(), &suggestions)
	c.JSON(http.StatusOK, gin.H{"data": suggestions, "count": len(suggestions)})
}

// PUT /queue/approve/:id
func (h *QueueHandler) Approve(c *gin.Context) {
	id, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	userID := c.GetString("userID")
	uid, _ := primitive.ObjectIDFromHex(userID)

	col := h.db.Collection(models.ColSuggestions)
	var suggestion models.SeoSuggestion
	if err := col.FindOne(context.Background(), bson.D{{Key: "_id", Value: id}}).Decode(&suggestion); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}

	now := time.Now()

	// Mark as approved (human decision recorded). Will upgrade to "live" only after successful push.
	col.UpdateByID(context.Background(), id, bson.D{{Key: "$set", Value: bson.D{
		{Key: "status", Value: models.SuggestionApproved},
		{Key: "reviewed_by", Value: uid},
		{Key: "reviewed_at", Value: now},
	}}})

	// Push to the appropriate backend (CMS / API / website).
	if err := h.execute.ApplySEOChange(&suggestion); err != nil {
		log.Printf("WARN: execute failed for %s: %v", suggestion.Page, err)
		// Stay in "approved" state — visible in dashboard so admin can retry or investigate.
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"status":  "approved",
			"warning": "Approved and saved. Live push failed: " + err.Error(),
		})
		return
	}

	// Push succeeded — mark live and log the change for impact tracking.
	col.UpdateByID(context.Background(), id, bson.D{{Key: "$set", Value: bson.D{
		{Key: "status", Value: models.SuggestionLive},
	}}})

	changeCol := h.db.Collection(models.ColChanges)
	changeCol.InsertOne(context.Background(), models.SeoChange{
		SuggestionID:    id,
		Page:            suggestion.Page,
		AppliedAt:       now,
		AppliedBy:       uid,
		BaselineMetrics: models.PageMetrics{},
		RollbackData:    suggestion.Current,
	})

	c.JSON(http.StatusOK, gin.H{"success": true, "status": "live"})
}

// PUT /queue/revert/:id — reverts a live change using stored rollback data
func (h *QueueHandler) Revert(c *gin.Context) {
	id, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	changeCol := h.db.Collection(models.ColChanges)
	var change models.SeoChange
	if err := changeCol.FindOne(context.Background(), bson.D{{Key: "suggestion_id", Value: id}}).Decode(&change); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no change record found for this suggestion"})
		return
	}

	if err := h.execute.Rollback(&change); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "rollback failed: " + err.Error()})
		return
	}

	// Mark suggestion back to pending
	h.db.Collection(models.ColSuggestions).UpdateByID(context.Background(), id, bson.D{{Key: "$set", Value: bson.D{
		{Key: "status", Value: models.SuggestionPending},
	}}})

	c.JSON(http.StatusOK, gin.H{"success": true, "reverted": true})
}

// PUT /queue/reject/:id
func (h *QueueHandler) Reject(c *gin.Context) {
	id, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	var body struct {
		Reason string `json:"reason"`
	}
	c.ShouldBindJSON(&body)

	userID := c.GetString("userID")
	uid, _ := primitive.ObjectIDFromHex(userID)
	now := time.Now()

	h.db.Collection(models.ColSuggestions).UpdateByID(context.Background(), id, bson.D{{Key: "$set", Value: bson.D{
		{Key: "status", Value: models.SuggestionRejected},
		{Key: "reviewed_by", Value: uid},
		{Key: "reviewed_at", Value: now},
		{Key: "approval_note", Value: body.Reason},
	}}})

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// GET /features — feature development pipeline
func (h *QueueHandler) ListFeatures(c *gin.Context) {
	col := h.db.Collection(models.ColFeatures)
	cursor, err := col.Find(context.Background(),
		bson.D{},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(20),
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var features []models.SeoFeature
	cursor.All(context.Background(), &features)
	c.JSON(http.StatusOK, gin.H{"data": features})
}

// PUT /features/:id/flag — set feature flag rollout %
func (h *QueueHandler) SetFeatureFlag(c *gin.Context) {
	featureID, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	var body struct {
		Enabled        bool `json:"enabled"`
		RolloutPercent int  `json:"rolloutPercent"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Get feature to find its flag key
	var feature models.SeoFeature
	h.db.Collection(models.ColFeatures).FindOne(context.Background(),
		bson.D{{Key: "_id", Value: featureID}},
	).Decode(&feature)

	if feature.Plan == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "feature has no plan"})
		return
	}

	h.db.Collection(models.ColFlags).UpdateOne(context.Background(),
		bson.D{{Key: "key", Value: feature.Plan.FeatureFlagKey}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "enabled", Value: body.Enabled},
			{Key: "rollout_percent", Value: body.RolloutPercent},
		}}},
	)

	// If enabling for first time, also update feature status to monitoring
	if body.Enabled {
		h.db.Collection(models.ColFeatures).UpdateByID(context.Background(), featureID, bson.D{{Key: "$set", Value: bson.D{
			{Key: "status", Value: models.FeatureMonitoring},
		}}})
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}
