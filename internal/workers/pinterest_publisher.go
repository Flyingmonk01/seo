package workers

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/91astro/seo-agent/internal/services"
)

// maxPinAttempts caps retries for a topic that keeps failing to pin, so a
// permanently broken topic (bad image, etc.) doesn't get retried forever.
const maxPinAttempts = 4

// StartPinterestPublisher runs a background loop that pins newly-approved blog
// posts to Pinterest. It mirrors the cms_topic_poller pattern: a human approves
// a topic in the CMS (status -> "approved"), and this loop picks it up — no
// inbound HTTP needed. Each topic is pinned at most once (guarded by pinId).
func (s *Server) StartPinterestPublisher() {
	if s.pinterest == nil || !s.pinterest.Enabled() {
		log.Println("[pinterest] disabled — publisher not started (set PINTEREST_ENABLED=true and credentials)")
		return
	}
	go func() {
		log.Println("[pinterest] Starting Pinterest publisher (60s interval)")
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			s.publishApprovedToPinterest()
		}
	}()
}

func (s *Server) publishApprovedToPinterest() {
	topics, err := s.cms.ListPinnableTopics(25)
	if err != nil {
		log.Printf("[pinterest] WARN: could not list pinnable topics: %v", err)
		return
	}
	if len(topics) == 0 {
		return
	}
	log.Printf("[pinterest] %d approved topic(s) to evaluate for pinning", len(topics))
	for _, t := range topics {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		s.pinTopic(ctx, t)
		cancel()
	}
}

// pinTopic publishes a single approved topic's blog post to Pinterest.
func (s *Server) pinTopic(ctx context.Context, t map[string]interface{}) {
	topicID, _ := t["id"].(string)
	if topicID == "" {
		return
	}

	// Defensive guards. ListPinnableTopics already filters on these, but the
	// worker must never re-pin or write to a topic outside its scope: a topic
	// that already has a pin, or one that is not approved. A loose/stale query
	// result must not cause a duplicate pin or an unrelated write.
	if pinID, _ := t["pinId"].(string); pinID != "" {
		return // already pinned — leave it alone
	}
	if status, _ := t["status"].(string); status != "approved" {
		return // not approved — not ours to touch
	}

	attempts := toInt(t["pinAttempts"])
	if attempts >= maxPinAttempts {
		return // exhausted — needs manual intervention (clear pinAttempts to retry)
	}

	postID := extractPostID(t)
	if postID == "" {
		s.recordPinFailure(topicID, attempts, "topic has no linked post")
		return
	}

	// depth=1 populates the post's featured image into a media object.
	post, err := s.cms.GetDocumentWithDepth("posts", postID, 1)
	if err != nil {
		s.recordPinFailure(topicID, attempts, fmt.Sprintf("fetch post: %v", err))
		return
	}
	if post == nil {
		s.recordPinFailure(topicID, attempts, "linked post not found")
		return
	}

	// Only pin posts that are publicly visible — never link a pin to a hidden
	// page. This is not a failure; we wait and re-check on the next poll.
	if hidden, _ := post["isHidden"].(bool); hidden {
		log.Printf("[pinterest] topic %s: post still hidden — waiting for it to be published", topicID)
		return
	}

	heading, _ := post["Heading"].(string)
	slug, _ := post["slug"].(string)
	if heading == "" || slug == "" {
		s.recordPinFailure(topicID, attempts, "post missing Heading or slug")
		return
	}

	imageURL := s.resolvePinImageURL(post["image"])
	if imageURL == "" {
		s.recordPinFailure(topicID, attempts, "post has no usable featured image")
		return
	}

	description := metaDescription(post)
	if description == "" {
		description = heading
	}

	link := fmt.Sprintf("%s/blogs/%s-%s", strings.TrimRight(s.cfg.WebsiteURL, "/"), slug, postID)

	res, err := s.pinterest.CreatePin(ctx, services.CreatePinInput{
		Title:       truncateRunes(heading, 100),
		Description: truncateRunes(description, 800),
		Link:        link,
		ImageURL:    imageURL,
		AltText:     truncateRunes(heading, 500),
	})
	if err != nil {
		s.recordPinFailure(topicID, attempts, err.Error())
		log.Printf("[pinterest] topic %s: pin failed (attempt %d/%d): %v", topicID, attempts+1, maxPinAttempts, err)
		return
	}

	if err := s.cms.UpdateTopic(topicID, map[string]interface{}{
		"pinId":    res.ID,
		"pinUrl":   res.URL,
		"pinnedAt": time.Now().Format(time.RFC3339),
		"pinError": "",
	}); err != nil {
		// Pin was created but we failed to record it — log loudly so it can be
		// reconciled manually (the pinId guard won't protect against a re-pin).
		log.Printf("[pinterest] WARN: topic %s pinned (%s) but CMS update failed: %v", topicID, res.ID, err)
		return
	}
	log.Printf("[pinterest] topic %s: pinned %q → %s", topicID, heading, res.URL)
}

// recordPinFailure increments the attempt counter and stores the error.
func (s *Server) recordPinFailure(topicID string, prevAttempts int, msg string) {
	if err := s.cms.UpdateTopic(topicID, map[string]interface{}{
		"pinAttempts": prevAttempts + 1,
		"pinError":    truncateRunes(msg, 500),
	}); err != nil {
		log.Printf("[pinterest] WARN: could not record pin failure for topic %s: %v", topicID, err)
	}
}

// resolvePinImageURL turns a depth-expanded CMS media object into a public URL.
// Prefers an absolute `url`, else builds one from the S3 base + `filename`.
func (s *Server) resolvePinImageURL(image interface{}) string {
	m, ok := image.(map[string]interface{})
	if !ok {
		return ""
	}
	if u, _ := m["url"].(string); strings.HasPrefix(u, "http") {
		return u
	}
	if fn, _ := m["filename"].(string); fn != "" {
		return strings.TrimRight(s.cfg.CMSMediaBaseURL, "/") + "/" + fn
	}
	return ""
}

// metaDescription pulls meta.description from a CMS post document.
func metaDescription(post map[string]interface{}) string {
	meta, ok := post["meta"].(map[string]interface{})
	if !ok {
		return ""
	}
	d, _ := meta["description"].(string)
	return d
}

// toInt coerces a JSON-decoded number (float64) to an int.
func toInt(v interface{}) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}

// truncateRunes trims a string to at most max runes (Pinterest field limits).
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimSpace(string(r[:max]))
}
