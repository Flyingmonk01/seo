package workers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/hibiken/asynq"
)

type regenerateBlogPayload struct {
	TopicID            string `json:"topic_id"`   // CMS seo-topics document ID
	Query              string `json:"query"`
	PostID             string `json:"post_id"`
	RegenerateContent  bool   `json:"regenerate_content"`
	RegenerateImage    bool   `json:"regenerate_image"`
	CustomInstructions string `json:"custom_instructions"`
}

func (s *Server) handleRegenerateBlog(ctx context.Context, task *asynq.Task) error {
	var p regenerateBlogPayload
	if err := json.Unmarshal(task.Payload(), &p); err != nil {
		return fmt.Errorf("parse regenerate payload: %w", err)
	}

	log.Printf("[regenerate] ── Regenerating post %s (content=%v, image=%v, query=%q) ──",
		p.PostID, p.RegenerateContent, p.RegenerateImage, p.Query)

	fields := map[string]interface{}{}

	// ── Regenerate content if requested ──────────────────────────────────────
	if p.RegenerateContent {
		post, err := s.generateBlogPostWithInstructions(ctx, p.Query, 100, p.CustomInstructions)
		if err != nil {
			log.Printf("[regenerate]   x content generation failed: %v", err)
			s.markTopicStatus(p.TopicID, "pending")
			return fmt.Errorf("regenerate content: %w", err)
		}

		fields["Heading"] = post.Heading
		fields["Category"] = post.Category
		fields["Content"] = post.Content
		fields["Paragraph"] = post.Paragraphs
		fields["meta"] = map[string]string{
			"title":       post.MetaTitle,
			"description": post.MetaDescription,
		}

		if catID := s.resolveCategoryID(post.Category); catID != "" {
			fields["category"] = catID
		}
		if authID := s.resolveAuthorID(); authID != "" {
			fields["author"] = authID
		}

		if post.ImagePrompt != "" && p.RegenerateImage {
			imgID, err := s.generateAndUploadImage(ctx, post.ImagePrompt, post.Heading)
			if err != nil {
				log.Printf("[regenerate]   ! image generation failed (keeping old): %v", err)
			} else {
				fields["image"] = imgID
				log.Printf("[regenerate]   + New image: %s", imgID)
			}
		}

		// Update heading and query in CMS topic
		s.cms.UpdateTopic(p.TopicID, map[string]interface{}{
			"heading": post.Heading,
			"query":   p.Query,
		})

		log.Printf("[regenerate]   + Content regenerated: %q", post.Heading)
	}

	// ── Regenerate image only ─────────────────────────────────────────────────
	if !p.RegenerateContent && p.RegenerateImage {
		doc, err := s.cms.GetFullDocument("Posts", p.PostID)
		if err != nil {
			s.markTopicStatus(p.TopicID, "pending")
			return fmt.Errorf("fetch post for image regen: %w", err)
		}
		heading, _ := doc["Heading"].(string)
		if heading == "" {
			heading = p.Query
		}

		imagePrompt := fmt.Sprintf("A visually stunning, professional blog featured image for an article about: %s. Indian Vedic astrology aesthetic, warm colors, celestial elements, no text in image.", p.Query)
		if p.CustomInstructions != "" {
			imagePrompt = fmt.Sprintf("%s Style guidance: %s", imagePrompt, p.CustomInstructions)
		}

		imgID, err := s.generateAndUploadImage(ctx, imagePrompt, heading)
		if err != nil {
			log.Printf("[regenerate]   x image generation failed: %v", err)
			s.markTopicStatus(p.TopicID, "pending")
			return fmt.Errorf("regenerate image: %w", err)
		}
		fields["image"] = imgID
		log.Printf("[regenerate]   + New image only: %s", imgID)
	}

	// Always hide post so it requires re-approval after regeneration
	fields["isHidden"] = true

	if len(fields) > 0 {
		if err := s.cms.PatchDocument("Posts", p.PostID, fields); err != nil {
			log.Printf("[regenerate]   x CMS update failed: %v", err)
			s.markTopicStatus(p.TopicID, "pending")
			return fmt.Errorf("cms update: %w", err)
		}
	}

	// Set status back to pending for re-review in CMS
	now := time.Now().Format(time.RFC3339)
	s.cms.UpdateTopic(p.TopicID, map[string]interface{}{
		"status":        "pending",
		"regeneratedAt": now,
		// Clear regeneration params
		"regenerateContent":  false,
		"regenerateImage":    false,
		"customTopic":        "",
		"customInstructions": "",
	})

	log.Printf("[regenerate] ── Done (pending review) ──")
	return nil
}

// markTopicStatus updates a CMS seo-topic's status field.
func (s *Server) markTopicStatus(topicID, status string) {
	if topicID == "" {
		return
	}
	s.cms.UpdateTopic(topicID, map[string]interface{}{"status": status})
}
