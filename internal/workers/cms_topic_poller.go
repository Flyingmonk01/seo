package workers

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/hibiken/asynq"
)

// StartCMSTopicPoller runs a background loop that polls CMS seo-topics for
// records with status="regenerating" and enqueues asynq regeneration tasks.
// The dashboard sets status=regenerating + stores params in the CMS record,
// then this poller picks it up — no inbound HTTP needed on the SEO server.
func (s *Server) StartCMSTopicPoller(redisAddr, redisPassword string) {
	go func() {
		log.Println("[cms-poller] Starting CMS topic poller (30s interval)")
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			s.pollRegeneratingTopics(redisAddr, redisPassword)
		}
	}()
}

func (s *Server) pollRegeneratingTopics(redisAddr, redisPassword string) {
	topics, err := s.cms.ListTopics("regenerating", 50)
	if err != nil {
		log.Printf("[cms-poller] WARN: could not fetch regenerating topics: %v", err)
		return
	}
	if len(topics) == 0 {
		return
	}

	log.Printf("[cms-poller] Found %d topics to regenerate", len(topics))

	client := asynq.NewClient(asynq.RedisClientOpt{
		Addr:     redisAddr,
		Password: redisPassword,
	})
	defer client.Close()

	for _, t := range topics {
		topicID, _ := t["id"].(string)
		if topicID == "" {
			continue
		}

		// Extract regeneration params stored by the dashboard
		postID := extractPostID(t)
		query, _ := t["query"].(string)
		if ct, ok := t["customTopic"].(string); ok && ct != "" {
			query = ct
		}
		regenContent, _ := t["regenerateContent"].(bool)
		regenImage, _ := t["regenerateImage"].(bool)
		customInstructions, _ := t["customInstructions"].(string)

		if !regenContent && !regenImage {
			// Nothing to do — reset to pending
			log.Printf("[cms-poller] Topic %s has regenerating status but no options set, resetting", topicID)
			s.cms.UpdateTopic(topicID, map[string]interface{}{"status": "pending"})
			continue
		}

		payload := map[string]interface{}{
			"topic_id":            topicID,
			"query":               query,
			"post_id":             postID,
			"regenerate_content":  regenContent,
			"regenerate_image":    regenImage,
			"custom_instructions": customInstructions,
		}
		data, _ := json.Marshal(payload)
		task := asynq.NewTask(TaskRegenerateBlog, data)

		if _, err := client.EnqueueContext(context.Background(), task,
			asynq.Queue("default"),
			asynq.MaxRetry(1),
			asynq.Unique(10*time.Minute), // prevent duplicate tasks for same topic
		); err != nil {
			log.Printf("[cms-poller] WARN: could not enqueue regenerate for topic %s: %v", topicID, err)
		} else {
			log.Printf("[cms-poller] Enqueued regeneration for topic %s (query=%q)", topicID, query)
		}
	}
}

// extractPostID pulls the post ID string from a CMS topic document.
// Payload CMS returns relationships as either a string ID or a nested object.
func extractPostID(topic map[string]interface{}) string {
	switch v := topic["post"].(type) {
	case string:
		return v
	case map[string]interface{}:
		if id, ok := v["id"].(string); ok {
			return id
		}
	}
	return ""
}
