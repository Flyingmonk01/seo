package workers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	openai "github.com/sashabaranov/go-openai"

	"github.com/91astro/seo-agent/internal/models"
	"github.com/91astro/seo-agent/internal/services"
)

type dailyBlogPayload struct {
	MaxPosts int `json:"max_posts"`
}

// loadUsedClusterKeysFromCMS loads cluster keys that are blocked from re-use.
// Rejected topics free up their cluster key so a different angle can be tried.
// Falls back to empty map on error so generation still proceeds.
func (s *Server) loadUsedClusterKeysFromCMS() map[string]bool {
	used := map[string]bool{}
	for _, status := range []string{"pending", "approved", "live", "regenerating"} {
		topics, err := s.cms.ListTopics(status, 1000)
		if err != nil {
			log.Printf("[daily-blog] WARN: could not load CMS topics (status=%s) for dedup: %v", status, err)
			continue
		}
		for _, t := range topics {
			if ck, ok := t["clusterKey"].(string); ok && ck != "" {
				used[ck] = true
			}
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
	log.Printf("[daily-blog] Creating up to %d new blog posts from trending GSC queries...", p.MaxPosts)

	// ── Step 1: Pull trending organic queries directly from GSC ───────────────
	//
	// We compare the last 7 days of GSC data against the prior 7 days and rank
	// by a volume-weighted growth score. This surfaces queries that are
	// actually rising in search demand right now — not stale long-tail gaps.
	trending, err := s.gsc.FetchTrendingQueries(ctx, 7, 20)
	if err != nil {
		return fmt.Errorf("daily-blog trending fetch: %w", err)
	}

	type gapCandidate struct {
		Query            string
		TotalImpressions int64
		TotalClicks      int64
		AvgPosition      float64
		GrowthRatio      float64
	}
	var gaps []gapCandidate
	candidateLimit := p.MaxPosts * 20
	for _, t := range trending {
		// Skip queries we already rank well for (top 3) — those don't need
		// a brand-new blog post; the existing page is already ranking.
		if t.AvgPosition > 0 && t.AvgPosition <= 3 {
			continue
		}
		gaps = append(gaps, gapCandidate{
			Query:            t.Query,
			TotalImpressions: t.RecentImpressions,
			TotalClicks:      t.RecentClicks,
			AvgPosition:      t.AvgPosition,
			GrowthRatio:      t.GrowthRatio,
		})
		if len(gaps) >= candidateLimit {
			break
		}
	}

	log.Printf("[daily-blog] %d trending queries from GSC (top candidates)", len(gaps))

	// ── Step 1b: Cluster sibling queries ─────────────────────────────────────
	//
	// Queries like "মকর রাশি", "মকর রাশিফল", "মকর রাশির রাশিফল" all share
	// the same cluster key. We bundle siblings with the strongest primary so
	// the enrichment + strategist see the full theme, not a lone query.
	clusterSiblings := map[string][]string{}
	primaryByCluster := map[string]string{}
	for _, g := range gaps {
		ck := services.ClusterKey(g.Query)
		if _, seen := primaryByCluster[ck]; !seen {
			primaryByCluster[ck] = g.Query
		} else if g.Query != primaryByCluster[ck] {
			clusterSiblings[ck] = append(clusterSiblings[ck], g.Query)
		}
	}

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
	var existingHeadingsList []string
	categoryIDCounts := map[string]int{}
	for _, post := range existingPosts {
		if h, ok := post["Heading"].(string); ok {
			existingHeadings[strings.ToLower(h)] = true
			existingHeadingsList = append(existingHeadingsList, h)
		}
		if catID, ok := post["category"].(string); ok && catID != "" {
			categoryIDCounts[catID]++
		}
	}

	// Fetch real categories from CMS and build name→count map using CMS IDs
	cmsCategoryMap := s.fetchCMSCategories()
	var cmsCategories []string
	categoryCounts := map[string]int{}
	for _, cat := range cmsCategoryMap {
		cmsCategories = append(cmsCategories, cat.Name)
		categoryCounts[strings.ToLower(cat.Name)] = categoryIDCounts[cat.ID]
	}
	log.Printf("[daily-blog] CMS categories: %v", cmsCategories)

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

		log.Printf("[daily-blog] ── Trending: %q (%d impr, pos %.1f, growth %+.0f%%) ──",
			gap.Query, gap.TotalImpressions, gap.AvgPosition, gap.GrowthRatio*100)

		// Enrich the theme with current-moment research (festivals, transits,
		// celebrity news) so the strategist can pick a time-anchored angle.
		// Soft-fails: if enrichment errors, we still generate (just less timely).
		siblings := clusterSiblings[clusterKey]
		enrich, err := s.enrichTheme(ctx, gap.Query, siblings)
		if err != nil {
			log.Printf("[daily-blog]   ! enrichment failed (continuing without it): %v", err)
			enrich = nil
		} else {
			log.Printf("[daily-blog]   * enrichment: archetype=%q anchor=%q",
				enrich.BestArchetype, enrich.RecommendedDateHook)
		}

		post, err := s.generateBlogPostEnriched(ctx, gap.Query, gap.TotalImpressions, existingHeadingsList, cmsCategories, categoryCounts, enrich)
		if err != nil {
			log.Printf("[daily-blog]   x generation failed: %v", err)
			continue
		}

		// Layer 2: CMS heading dedup
		if existingHeadings[strings.ToLower(post.Heading)] {
			log.Printf("[daily-blog]   ~ skip — heading already exists: %q", post.Heading)
			continue
		}

		// CMS Posts requires `image`, so we must produce one. Fall back to a
		// heading-derived prompt when the strategist LLM omitted imagePrompt,
		// and skip the post entirely if generation/upload fails (don't push
		// payloads the CMS will reject).
		imagePrompt := post.ImagePrompt
		if imagePrompt == "" {
			imagePrompt = fmt.Sprintf("Warm realistic Indian spiritual scene illustrating: %s. Soft golden lighting, temple or puja setting, no text.", post.Heading)
			log.Printf("[daily-blog]   ~ strategist omitted imagePrompt — using heading-based fallback")
		}
		imageID, err := s.generateAndUploadImage(ctx, imagePrompt, post.Heading)
		if err != nil {
			log.Printf("[daily-blog]   x image generation failed, skipping post (CMS requires image): %v", err)
			continue
		}
		log.Printf("[daily-blog]   + Featured image uploaded: %s", imageID)

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

// generateAndUploadImage creates a featured image via gpt-image-1 and uploads
// it to the CMS Media collection. Returns the media document ID.
// gpt-image-1 always returns base64 — it does not support response_format=url,
// style, or quality=standard/hd.
func (s *Server) generateAndUploadImage(ctx context.Context, imagePrompt, heading string) (string, error) {
	imgResp, err := s.openai.Client().CreateImage(ctx, openai.ImageRequest{
		Prompt:  imagePrompt,
		Model:   "gpt-image-1",
		N:       1,
		Size:    "1536x1024", // landscape; gpt-image-1 does not support 1792x1024
		Quality: "high",      // gpt-image-1 accepts low|medium|high|auto
	})
	if err != nil {
		return "", fmt.Errorf("gpt-image-1 generation: %w", err)
	}
	if len(imgResp.Data) == 0 || imgResp.Data[0].B64JSON == "" {
		return "", fmt.Errorf("gpt-image-1 returned no image data")
	}

	imageData, err := base64.StdEncoding.DecodeString(imgResp.Data[0].B64JSON)
	if err != nil {
		return "", fmt.Errorf("decode base64 image: %w", err)
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
