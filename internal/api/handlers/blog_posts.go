package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/91astro/seo-agent/internal/models"
	"github.com/91astro/seo-agent/internal/services"
)

type BlogPostsHandler struct {
	db            *mongo.Database
	cms           *services.CMSService
	redisAddr     string
	redisPassword string
}

func NewBlogPostsHandler(db *mongo.Database, cms *services.CMSService, redisAddr, redisPassword string) *BlogPostsHandler {
	return &BlogPostsHandler{db: db, cms: cms, redisAddr: redisAddr, redisPassword: redisPassword}
}

// GET /blog-posts — list all generated blog posts with their status
func (h *BlogPostsHandler) ListBlogPosts(c *gin.Context) {
	col := h.db.Collection(models.ColBlogTopics)

	status := c.DefaultQuery("status", "all")

	filter := bson.D{}
	if status != "all" {
		filter = bson.D{{Key: "status", Value: status}}
	}

	cursor, err := col.Find(context.Background(), filter,
		options.Find().
			SetSort(bson.D{{Key: "created_at", Value: -1}}).
			SetLimit(100),
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var topics []models.SeoBlogTopic
	if err := cursor.All(context.Background(), &topics); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": topics, "count": len(topics)})
}

// PUT /blog-posts/approve/:id — unhide the post in CMS (make it live)
func (h *BlogPostsHandler) ApproveBlogPost(c *gin.Context) {
	id, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	// Find the topic record
	col := h.db.Collection(models.ColBlogTopics)
	var topic models.SeoBlogTopic
	if err := col.FindOne(context.Background(), bson.D{{Key: "_id", Value: id}}).Decode(&topic); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "blog topic not found"})
		return
	}

	// Unhide the post in CMS (set isHidden to false)
	if err := h.cms.PatchDocument("Posts", topic.PostID, map[string]interface{}{
		"isHidden": false,
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "CMS update failed: " + err.Error()})
		return
	}

	// Update status in our tracking collection
	now := time.Now()
	col.UpdateByID(context.Background(), id, bson.D{{Key: "$set", Value: bson.D{
		{Key: "status", Value: "approved"},
		{Key: "approved_at", Value: now},
	}}})

	c.JSON(http.StatusOK, gin.H{"success": true, "status": "approved"})
}

// PUT /blog-posts/reject/:id — mark as rejected, optionally delete from CMS
func (h *BlogPostsHandler) RejectBlogPost(c *gin.Context) {
	id, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	var body struct {
		Reason    string `json:"reason"`
		DeleteCMS bool   `json:"deleteCms"`
	}
	c.ShouldBindJSON(&body)

	col := h.db.Collection(models.ColBlogTopics)
	var topic models.SeoBlogTopic
	if err := col.FindOne(context.Background(), bson.D{{Key: "_id", Value: id}}).Decode(&topic); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "blog topic not found"})
		return
	}

	now := time.Now()
	col.UpdateByID(context.Background(), id, bson.D{{Key: "$set", Value: bson.D{
		{Key: "status", Value: "rejected"},
		{Key: "rejected_at", Value: now},
		{Key: "reject_reason", Value: body.Reason},
	}}})

	c.JSON(http.StatusOK, gin.H{"success": true, "status": "rejected"})
}

// PUT /blog-posts/revert/:id — hide an approved post back (make it not visible on website)
func (h *BlogPostsHandler) RevertBlogPost(c *gin.Context) {
	id, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	col := h.db.Collection(models.ColBlogTopics)
	var topic models.SeoBlogTopic
	if err := col.FindOne(context.Background(), bson.D{{Key: "_id", Value: id}}).Decode(&topic); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "blog topic not found"})
		return
	}

	// Hide the post in CMS again
	if err := h.cms.PatchDocument("Posts", topic.PostID, map[string]interface{}{
		"isHidden": true,
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "CMS update failed: " + err.Error()})
		return
	}

	// Set status back to pending
	col.UpdateByID(context.Background(), id, bson.D{{Key: "$set", Value: bson.D{
		{Key: "status", Value: "pending"},
		{Key: "approved_at", Value: nil},
	}}})

	c.JSON(http.StatusOK, gin.H{"success": true, "status": "pending"})
}

// PUT /blog-posts/regenerate/:id — re-generate content for a blog post using the same query
func (h *BlogPostsHandler) RegenerateBlogPost(c *gin.Context) {
	id, err := primitive.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	// Read body FIRST — Gin can only read it once
	var body struct {
		RegenerateContent  bool   `json:"regenerateContent"`
		RegenerateImage    bool   `json:"regenerateImage"`
		CustomTopic        string `json:"customTopic"`
		CustomInstructions string `json:"customInstructions"`
	}
	c.ShouldBindJSON(&body)

	col := h.db.Collection(models.ColBlogTopics)
	var topic models.SeoBlogTopic
	if err := col.FindOne(context.Background(), bson.D{{Key: "_id", Value: id}}).Decode(&topic); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "blog topic not found"})
		return
	}

	// Enqueue a regenerate task for the worker to pick up
	client := asynq.NewClient(asynq.RedisClientOpt{
		Addr:     h.redisAddr,
		Password: h.redisPassword,
	})
	defer client.Close()

	query := topic.Query
	if body.CustomTopic != "" {
		query = body.CustomTopic
	}

	payload := map[string]interface{}{
		"topic_id":            id.Hex(),
		"query":               query,
		"post_id":             topic.PostID,
		"regenerate_content":  body.RegenerateContent,
		"regenerate_image":    body.RegenerateImage,
		"custom_instructions": body.CustomInstructions,
	}
	data, _ := json.Marshal(payload)
	task := asynq.NewTask("seo:regenerate_blog", data)

	_, err = client.Enqueue(task, asynq.Queue("default"), asynq.MaxRetry(1))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to enqueue regenerate task: " + err.Error()})
		return
	}

	// Mark as regenerating
	col.UpdateByID(context.Background(), id, bson.D{{Key: "$set", Value: bson.D{
		{Key: "status", Value: "regenerating"},
	}}})

	c.JSON(http.StatusOK, gin.H{"success": true, "status": "regenerating"})
}
