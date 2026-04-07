package workers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/hibiken/asynq"

	openai "github.com/sashabaranov/go-openai"
)

type internalLinkPayload struct {
	MaxLinks int `json:"max_links"`
}

func (s *Server) handleInternalLink(ctx context.Context, task *asynq.Task) error {
	var p internalLinkPayload
	if err := json.Unmarshal(task.Payload(), &p); err != nil || p.MaxLinks == 0 {
		p.MaxLinks = 10
	}

	log.Println("[linker] ─────────────────────────────────────────")
	log.Printf("[linker] Adding up to %d internal links...", p.MaxLinks)

	// Fetch all posts from CMS
	posts, err := s.cms.ListPosts(500, "en")
	if err != nil {
		return fmt.Errorf("list posts: %w", err)
	}

	log.Printf("[linker] Fetched %d posts from CMS", len(posts))

	// Build a simple index: postID → {heading, slug, id, category, paragraphCount, hasLinks}
	type postInfo struct {
		ID             string
		Heading        string
		Slug           string
		Category       string
		ParagraphCount int
		HasLinks       bool // any paragraph already has a Hyperlink
	}
	var indexed []postInfo
	for _, post := range posts {
		id, _ := post["id"].(string)
		heading, _ := post["Heading"].(string)
		slug, _ := post["slug"].(string)

		catStr := ""
		if cat, ok := post["category"].(map[string]interface{}); ok {
			catStr, _ = cat["id"].(string)
		} else if cat, ok := post["category"].(string); ok {
			catStr = cat
		}

		paragraphs, _ := post["Paragraph"].([]interface{})
		hasLinks := false
		for _, p := range paragraphs {
			if pm, ok := p.(map[string]interface{}); ok {
				if h, ok := pm["Hyperlink"].(string); ok && h != "" {
					hasLinks = true
					break
				}
			}
		}

		if id != "" && heading != "" {
			indexed = append(indexed, postInfo{
				ID:             id,
				Heading:        heading,
				Slug:           slug,
				Category:       catStr,
				ParagraphCount: len(paragraphs),
				HasLinks:       hasLinks,
			})
		}
	}

	// Find posts WITHOUT internal links (orphans) — these are link targets
	var orphans []postInfo
	for _, p := range indexed {
		if !p.HasLinks {
			orphans = append(orphans, p)
		}
	}
	log.Printf("[linker] Found %d posts without internal links", len(orphans))

	if len(orphans) == 0 {
		log.Println("[linker] No orphan posts to link — done")
		return nil
	}

	// For each orphan, find related posts in same category to be the link SOURCE
	linked := 0
	for _, orphan := range orphans {
		if linked >= p.MaxLinks {
			break
		}

		// Find a source post in the same category that has paragraphs
		var source *postInfo
		for i, candidate := range indexed {
			if candidate.ID == orphan.ID {
				continue
			}
			if candidate.Category == orphan.Category && candidate.ParagraphCount > 0 {
				source = &indexed[i]
				break
			}
		}
		if source == nil {
			// Fall back to any post with paragraphs
			for i, candidate := range indexed {
				if candidate.ID == orphan.ID {
					continue
				}
				if candidate.ParagraphCount > 0 {
					source = &indexed[i]
					break
				}
			}
		}
		if source == nil {
			continue
		}

		// Generate anchor text via GPT-4o
		anchor, err := s.generateAnchorText(ctx, source.Heading, orphan.Heading)
		if err != nil {
			log.Printf("[linker]   ✗ anchor gen failed: %v", err)
			continue
		}

		// Build target URL
		targetURL := fmt.Sprintf("/blogs/%s-%s", orphan.Slug, orphan.ID)

		// Add link to first paragraph of source post
		err = s.cms.AddInternalLink(source.ID, 0, targetURL, orphan.ID)
		if err != nil {
			log.Printf("[linker]   ✗ CMS link failed: %v", err)
			continue
		}

		log.Printf("[linker]   ✓ %q → %q (anchor: %q)", source.Heading, orphan.Heading, anchor)
		_ = anchor // anchor is for logging; the Hyperlink field is the URL
		linked++
	}

	log.Printf("[linker] ── Summary: added %d internal links ──", linked)
	log.Println("[linker] ─────────────────────────────────────────")
	return nil
}

func (s *Server) generateAnchorText(ctx context.Context, sourceTitle, targetTitle string) (string, error) {
	prompt := fmt.Sprintf(`Generate a natural anchor text (2-5 words) for an internal link.

Source article: "%s"
Target article (being linked to): "%s"

Output ONLY the anchor text string, nothing else. No quotes, no JSON.
Example: "daily Aries predictions"`,
		sourceTitle, targetTitle,
	)

	resp, err := s.openai.Client().CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: s.cfg.OpenAIModel,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleUser, Content: prompt},
		},
		Temperature: 0.3,
		MaxTokens:   20,
	})
	if err != nil {
		return "", err
	}

	anchor := strings.TrimSpace(resp.Choices[0].Message.Content)
	anchor = strings.Trim(anchor, "\"'")
	return anchor, nil
}
