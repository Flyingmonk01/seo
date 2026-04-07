package workers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	openai "github.com/sashabaranov/go-openai"
	"go.mongodb.org/mongo-driver/bson"

	"github.com/91astro/seo-agent/internal/models"
	"github.com/91astro/seo-agent/internal/services"
)

type dailyBlogPayload struct {
	MaxPosts int `json:"max_posts"`
}

// loadUsedClusterKeysFromCMS loads all cluster keys already used for blog topics from CMS.
// Falls back to empty map on error so generation still proceeds.
func (s *Server) loadUsedClusterKeysFromCMS() map[string]bool {
	topics, err := s.cms.ListTopics("", 1000)
	used := map[string]bool{}
	if err != nil {
		log.Printf("[daily-blog] WARN: could not load CMS topics for dedup: %v", err)
		return used
	}
	for _, t := range topics {
		if ck, ok := t["clusterKey"].(string); ok && ck != "" {
			used[ck] = true
		}
	}
	return used
}

func (s *Server) handleDailyBlogCreate(ctx context.Context, task *asynq.Task) error {
	var p dailyBlogPayload
	if err := json.Unmarshal(task.Payload(), &p); err != nil || p.MaxPosts == 0 {
		p.MaxPosts = 5
	}

	log.Println("[daily-blog] ─────────────────────────────────────────")
	log.Printf("[daily-blog] Creating up to %d new blog posts from content gaps...", p.MaxPosts)

	// ── Step 1: Find content gaps from GSC data ──────────────────────────────

	rawCol := s.db.Collection(models.ColRawData)
	candidateLimit := int64(p.MaxPosts * 5)

	pipeline := []bson.D{
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$query"},
			{Key: "totalImpressions", Value: bson.D{{Key: "$sum", Value: "$impressions"}}},
			{Key: "totalClicks", Value: bson.D{{Key: "$sum", Value: "$clicks"}}},
			{Key: "avgPosition", Value: bson.D{{Key: "$avg", Value: "$position"}}},
			{Key: "pages", Value: bson.D{{Key: "$addToSet", Value: "$page"}}},
		}}},
		{{Key: "$match", Value: bson.D{
			{Key: "totalImpressions", Value: bson.D{{Key: "$gte", Value: 50}}},
			{Key: "avgPosition", Value: bson.D{{Key: "$gte", Value: 8}}},
		}}},
		{{Key: "$sort", Value: bson.D{{Key: "totalImpressions", Value: -1}}}},
		{{Key: "$limit", Value: candidateLimit}},
	}

	cursor, err := rawCol.Aggregate(ctx, pipeline)
	if err != nil {
		return fmt.Errorf("daily-blog content gap aggregate: %w", err)
	}
	defer cursor.Close(ctx)

	var gaps []struct {
		Query            string   `bson:"_id"`
		TotalImpressions int64    `bson:"totalImpressions"`
		TotalClicks      int64    `bson:"totalClicks"`
		AvgPosition      float64  `bson:"avgPosition"`
		Pages            []string `bson:"pages"`
	}
	if err := cursor.All(ctx, &gaps); err != nil {
		return err
	}

	log.Printf("[daily-blog] Found %d potential content gaps", len(gaps))

	// ── Step 2: Build dedup sets ─────────────────────────────────────────────

	// Layer 1: Load used cluster keys from CMS seo-topics
	usedClusterKeys := s.loadUsedClusterKeysFromCMS()
	log.Printf("[daily-blog] %d cluster keys already used in previous runs", len(usedClusterKeys))

	// Layer 2: Load existing CMS post headings
	existingPosts, err := s.cms.ListPosts(500, "en")
	if err != nil {
		log.Printf("[daily-blog] WARN: could not list existing posts: %v", err)
	}
	existingHeadings := map[string]bool{}
	for _, post := range existingPosts {
		if h, ok := post["Heading"].(string); ok {
			existingHeadings[strings.ToLower(h)] = true
		}
	}

	// ── Step 3: Filter gaps and generate posts ────────────────────────────────

	created := 0
	for _, gap := range gaps {
		if created >= p.MaxPosts {
			break
		}

		if len(gap.Query) < 10 {
			continue
		}

		// Layer 3: Cluster key dedup
		clusterKey := services.ClusterKey(gap.Query)
		if usedClusterKeys[clusterKey] {
			log.Printf("[daily-blog]   ~ skip %q — cluster key %q already used", gap.Query, clusterKey)
			continue
		}

		log.Printf("[daily-blog] ── Gap: %q (%d impressions, pos %.1f) ──",
			gap.Query, gap.TotalImpressions, gap.AvgPosition)

		post, err := s.generateBlogPost(ctx, gap.Query, gap.TotalImpressions)
		if err != nil {
			log.Printf("[daily-blog]   x generation failed: %v", err)
			continue
		}

		// Layer 2: CMS heading dedup
		if existingHeadings[strings.ToLower(post.Heading)] {
			log.Printf("[daily-blog]   ~ skip — heading already exists: %q", post.Heading)
			continue
		}

		var imageID string
		if post.ImagePrompt != "" {
			imgID, err := s.generateAndUploadImage(ctx, post.ImagePrompt, post.Heading)
			if err != nil {
				log.Printf("[daily-blog]   ! image generation failed (continuing without image): %v", err)
			} else {
				imageID = imgID
				log.Printf("[daily-blog]   + Featured image uploaded: %s", imageID)
			}
		}

		categoryRelID := s.resolveCategoryID(post.Category)
		authorID := s.resolveAuthorID()

		cmsPost := map[string]interface{}{
			"title":      post.Heading,
			"Heading":    post.Heading,
			"Date":       time.Now().Format("2 January 2006"),
			"Category":   post.Category,
			"Content":    post.Content,
			"Paragraph":  post.Paragraphs,
			"Identifier": "en",
			"isHidden":   true,
			"meta": map[string]string{
				"title":       post.MetaTitle,
				"description": post.MetaDescription,
			},
		}
		if imageID != "" {
			cmsPost["image"] = imageID
		}
		if authorID != "" {
			cmsPost["author"] = authorID
		}
		if categoryRelID != "" {
			cmsPost["category"] = categoryRelID
		}

		docID, err := s.cms.CreatePost(cmsPost)
		if err != nil {
			log.Printf("[daily-blog]   x CMS create failed: %v", err)
			continue
		}

		log.Printf("[daily-blog]   + Created post %s: %q (hidden, needs review)", docID, post.Heading)

		// Record topic in CMS seo-topics (replaces MongoDB seo_blog_topics)
		topicID, err := s.cms.CreateTopic(map[string]interface{}{
			"query":      gap.Query,
			"clusterKey": clusterKey,
			"heading":    post.Heading,
			"post":       docID,
			"status":     "pending",
		})
		if err != nil {
			log.Printf("[daily-blog]   ! WARN: could not save topic to CMS: %v", err)
		} else {
			log.Printf("[daily-blog]   + Topic saved to CMS: %s", topicID)
		}

		usedClusterKeys[clusterKey] = true
		existingHeadings[strings.ToLower(post.Heading)] = true

		// Keep seo_suggestions for pipeline 1 (page optimization dashboard)
		s.db.Collection(models.ColSuggestions).InsertOne(ctx, models.SeoSuggestion{
			Page:       fmt.Sprintf("new-post:%s", docID),
			Locale:     "en",
			PageSource: models.PageSourceCMS,
			CMSPageID:  docID,
			Proposed: models.SEOContent{
				Title:           post.MetaTitle,
				MetaDescription: post.MetaDescription,
			},
			GeneratedBy: s.cfg.OpenAIModel,
			Status:      models.SuggestionPending,
			CreatedAt:   time.Now(),
		})

		created++
	}

	log.Printf("[daily-blog] ── Summary: created %d new blog posts (hidden) ──", created)
	log.Println("[daily-blog] ─────────────────────────────────────────")
	return nil
}

// generateAndUploadImage creates a featured image via DALL-E 3, downloads it,
// and uploads it to the CMS Media collection. Returns the media document ID.
func (s *Server) generateAndUploadImage(ctx context.Context, imagePrompt, heading string) (string, error) {
	imgResp, err := s.openai.Client().CreateImage(ctx, openai.ImageRequest{
		Prompt:         imagePrompt,
		Model:          openai.CreateImageModelDallE3,
		N:              1,
		Size:           openai.CreateImageSize1792x1024,
		Quality:        openai.CreateImageQualityStandard,
		Style:          openai.CreateImageStyleNatural,
		ResponseFormat: openai.CreateImageResponseFormatURL,
	})
	if err != nil {
		return "", fmt.Errorf("dall-e generation: %w", err)
	}
	if len(imgResp.Data) == 0 {
		return "", fmt.Errorf("dall-e returned no images")
	}

	httpResp, err := http.Get(imgResp.Data[0].URL)
	if err != nil {
		return "", fmt.Errorf("download generated image: %w", err)
	}
	defer httpResp.Body.Close()

	imageData, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return "", fmt.Errorf("read image body: %w", err)
	}

	slug := strings.ToLower(strings.ReplaceAll(heading, " ", "-"))
	if len(slug) > 50 {
		slug = slug[:50]
	}
	filename := fmt.Sprintf("%s-%d.png", slug, time.Now().Unix())

	mediaID, err := s.cms.UploadMedia(imageData, filename, heading)
	if err != nil {
		return "", fmt.Errorf("cms media upload: %w", err)
	}
	return mediaID, nil
}
