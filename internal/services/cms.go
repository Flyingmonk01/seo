package services

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// CMSService interacts with Payload CMS at cms1.91astrology.com.
type CMSService struct {
	baseURL    string
	email      string
	password   string
	httpClient *http.Client

	// Token cache — avoids re-login on every API call.
	// Payload JWT tokens expire after 2 hours by default.
	mu         sync.Mutex
	token      string
	tokenExpAt time.Time
}

func NewCMSService(baseURL, email, password string) *CMSService {
	return &CMSService{
		baseURL:    strings.TrimRight(baseURL, "/"),
		email:      email,
		password:   password,
		httpClient: &http.Client{},
	}
}

// IsConfigured returns true when CMS credentials are set.
func (c *CMSService) IsConfigured() bool {
	return c.baseURL != "" && c.email != "" && c.password != ""
}

// GetToken returns a cached JWT token, refreshing only when expired.
// Payload CMS tokens expire after 2 hours; we refresh at 1h50m to be safe.
func (c *CMSService) GetToken() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.token != "" && time.Now().Before(c.tokenExpAt) {
		return c.token, nil
	}

	token, err := c.login()
	if err != nil {
		return "", err
	}

	c.token = token
	fmt.Println("CMSService: obtained new token, expires in 2 hours ", token)
	c.tokenExpAt = time.Now().Add(110 * time.Minute) // refresh 10min before 2hr expiry
	return token, nil
}

// login authenticates with Payload CMS and returns a fresh JWT token.
func (c *CMSService) login() (string, error) {
	payload := map[string]string{"email": c.email, "password": c.password}
	body, _ := json.Marshal(payload)

	resp, err := c.httpClient.Post(c.baseURL+"/api/users/login", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("cms login: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("cms login returned %d", resp.StatusCode)
	}

	var result struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("cms login decode: %w", err)
	}
	if result.Token == "" {
		return "", fmt.Errorf("cms login: empty token")
	}
	return result.Token, nil
}

// Login is kept for backwards compatibility. Use GetToken() instead.
func (c *CMSService) Login() (string, error) {
	return c.GetToken()
}

// CMSDoc holds the fields we need from any Payload CMS document.
type CMSDoc struct {
	ID      string `json:"id"`
	Slug    string `json:"slug"`
	Heading string `json:"Heading"` // used by Posts collection
	Meta    struct {
		Title       string `json:"title"`
		Description string `json:"description"`
	} `json:"meta"`
}

// CMSTarget describes which collection a URL maps to and the doc ID to patch.
type CMSTarget struct {
	Collection string // e.g. "pages", "Posts"
	DocID      string // Payload CMS document _id
	// UpdateFields to PATCH — caller populates this
	Fields map[string]interface{}
}

// objectIDRegex matches a 24-char hex Payload/MongoDB ObjectID.
var objectIDRegex = regexp.MustCompile(`[0-9a-f]{24}`)

// ResolveTarget determines the CMS collection and document ID for a page URL.
// It uses URL patterns first (fast), then falls back to a slug lookup.
func (c *CMSService) ResolveTarget(pageURL string) (*CMSTarget, error) {
	parsed, err := url.Parse(pageURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	path := strings.Trim(parsed.Path, "/")
	segments := strings.Split(path, "/")

	// Strip locale prefix (2-char codes: en, hi, ta, bn, es …)
	if len(segments) > 0 && len(segments[0]) == 2 {
		segments = segments[1:]
	}

	if len(segments) == 0 {
		return nil, fmt.Errorf("cannot resolve CMS target for root URL")
	}

	// ── Blog posts: /blogs/<title-slug-with-objectid-at-end> ─────────────────
	// The Payload document ID is always the last 24-char hex segment of the slug.
	if segments[0] == "blogs" && len(segments) > 1 {
		fullSlug := segments[len(segments)-1]
		parts := strings.Split(fullSlug, "-")
		for i := len(parts) - 1; i >= 0; i-- {
			if objectIDRegex.MatchString(parts[i]) {
				return &CMSTarget{Collection: "Posts", DocID: parts[i]}, nil
			}
		}
		return nil, fmt.Errorf("could not extract ObjectID from blog URL: %s", pageURL)
	}

	// ── Dynamic entity pages (astrologer, celebrity) — live in 91astro-api ──
	// These have a UUID at the end: /astrologer/detail/<uuid> or /celebrity/<name>-<uuid>
	uuidRegex := regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	lastSegment := segments[len(segments)-1]
	if uuidRegex.MatchString(lastSegment) {
		return nil, fmt.Errorf("page %s is a dynamic entity page (astrologer/celebrity) — needs 91astro-api internal endpoint", pageURL)
	}

	// ── Regular widget / tool pages: slug lookup in pages collection ──────────
	slug := segments[len(segments)-1]
	doc, found, err := c.FindInCollection("pages", slug)
	if err != nil {
		return nil, err
	}
	if found {
		return &CMSTarget{Collection: "pages", DocID: doc.ID}, nil
	}

	return nil, fmt.Errorf("page not found in CMS: %s", pageURL)
}

// FindInCollection queries a Payload collection by slug.
func (c *CMSService) FindInCollection(collection, slug string) (*CMSDoc, bool, error) {
	q := url.Values{}
	q.Set("where[slug][equals]", slug)
	q.Set("depth", "0")
	q.Set("limit", "1")

	reqURL := fmt.Sprintf("%s/api/%s?%s", c.baseURL, collection, q.Encode())
	resp, err := c.httpClient.Get(reqURL)
	if err != nil {
		return nil, false, fmt.Errorf("cms find %s: %w", collection, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return nil, false, fmt.Errorf("cms find %s returned %d", collection, resp.StatusCode)
	}

	var result struct {
		Docs []CMSDoc `json:"docs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, false, err
	}
	if len(result.Docs) == 0 {
		return nil, false, nil
	}
	return &result.Docs[0], true, nil
}

// PatchDocument patches arbitrary fields on a CMS document.
// fields is a map of top-level field names → values to merge.
func (c *CMSService) PatchDocument(collection, docID string, fields map[string]interface{}) error {
	token, err := c.Login()
	if err != nil {
		return err
	}

	body, _ := json.Marshal(fields)
	req, err := http.NewRequest(http.MethodPatch,
		fmt.Sprintf("%s/api/%s/%s", c.baseURL, collection, docID),
		bytes.NewReader(body),
	)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "JWT "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("cms patch %s/%s: %w", collection, docID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		var errBody map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&errBody)
		return fmt.Errorf("cms patch returned %d: %v", resp.StatusCode, errBody)
	}
	return nil
}

// UpdatePageMeta is a convenience wrapper for pages with a meta block.
func (c *CMSService) UpdatePageMeta(pageID, title, description string) error {
	return c.PatchDocument("pages", pageID, map[string]interface{}{
		"meta": map[string]string{
			"title":       title,
			"description": description,
		},
	})
}

// UpdatePostMeta updates a blog post — title goes to both Heading and meta.title,
// description goes to meta.description.
func (c *CMSService) UpdatePostMeta(postID, title, description string) error {
	return c.PatchDocument("Posts", postID, map[string]interface{}{
		"Heading": title,
		"meta": map[string]string{
			"title":       title,
			"description": description,
		},
	})
}

// GetDocumentByID fetches a single CMS document by its ID.
func (c *CMSService) GetDocumentByID(collection, docID string) (*CMSDoc, error) {
	reqURL := fmt.Sprintf("%s/api/%s/%s?depth=0", c.baseURL, collection, docID)
	resp, err := c.httpClient.Get(reqURL)
	if err != nil {
		return nil, fmt.Errorf("cms get %s/%s: %w", collection, docID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return nil, nil
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("cms get %s/%s returned %d", collection, docID, resp.StatusCode)
	}

	var doc CMSDoc
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

// FindPageBySlug is kept for backwards-compat with existing callers.
func (c *CMSService) FindPageBySlug(slug string) (*CMSDoc, bool, error) {
	return c.FindInCollection("pages", slug)
}

// SlugFromURL extracts the last meaningful path segment from a full page URL.
// e.g. "https://www.91astrology.com/en/spouse-name" → "spouse-name"
func SlugFromURL(pageURL string) string {
	parsed, err := url.Parse(pageURL)
	if err != nil {
		return pageURL
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	// strip locale prefix (2-letter codes like "en", "hi")
	filtered := parts[:0]
	for _, p := range parts {
		if len(p) == 2 && p >= "aa" && p <= "zz" {
			continue
		}
		if p != "" {
			filtered = append(filtered, p)
		}
	}
	if len(filtered) == 0 {
		return ""
	}
	return filtered[len(filtered)-1]
}

// ── New methods for content operations ───────────────────────────────────────

// GetFullDocument fetches a CMS document with all fields as raw JSON.
func (c *CMSService) GetFullDocument(collection, docID string) (map[string]interface{}, error) {
	reqURL := fmt.Sprintf("%s/api/%s/%s?depth=0", c.baseURL, collection, docID)
	resp, err := c.httpClient.Get(reqURL)
	if err != nil {
		return nil, fmt.Errorf("cms get full %s/%s: %w", collection, docID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("cms get full %s/%s returned %d", collection, docID, resp.StatusCode)
	}

	var doc map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// AddFAQToPage appends a FAQ block to a page's layout array.
func (c *CMSService) AddFAQToPage(pageID string, faqs []FAQBlockItem) error {
	doc, err := c.GetFullDocument("pages", pageID)
	if err != nil {
		return fmt.Errorf("fetch page for FAQ: %w", err)
	}

	layout, _ := doc["layout"].([]interface{})

	// Build FAQ entries matching CMS block schema
	faqEntries := make([]map[string]interface{}, len(faqs))
	for i, faq := range faqs {
		faqEntries[i] = map[string]interface{}{
			"Question": faq.Question,
			"Answer":   []map[string]string{{"Answer": faq.Answer}},
		}
	}

	faqBlock := map[string]interface{}{
		"blockType": "FAQ",
		"FAQ":       faqEntries,
	}

	layout = append(layout, faqBlock)

	return c.PatchDocument("pages", pageID, map[string]interface{}{
		"layout": layout,
	})
}

// FAQBlockItem matches the CMS FAQ block structure.
type FAQBlockItem struct {
	Question string
	Answer   string
}

// CreatePost creates a new blog post in the Posts collection.
// Returns the created document ID.
func (c *CMSService) CreatePost(post map[string]interface{}) (string, error) {
	token, err := c.GetToken()
	if err != nil {
		return "", err
	}

	body, _ := json.Marshal(post)
	req, err := http.NewRequest(http.MethodPost,
		c.baseURL+"/api/Posts",
		bytes.NewReader(body),
	)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "JWT "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("cms create post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		var errBody map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&errBody)
		return "", fmt.Errorf("cms create post returned %d: %v", resp.StatusCode, errBody)
	}

	var result struct {
		Doc struct {
			ID string `json:"id"`
		} `json:"doc"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("cms create post decode: %w", err)
	}
	return result.Doc.ID, nil
}

// UpdatePostContent updates the Content and Paragraph fields of a blog post.
func (c *CMSService) UpdatePostContent(postID string, content interface{}, paragraphs interface{}) error {
	fields := map[string]interface{}{}
	if content != nil {
		fields["Content"] = content
	}
	if paragraphs != nil {
		fields["Paragraph"] = paragraphs
	}
	if len(fields) == 0 {
		return nil
	}
	return c.PatchDocument("Posts", postID, fields)
}

// AddInternalLink sets the Hyperlink and BlogToLink fields on a specific paragraph of a post.
func (c *CMSService) AddInternalLink(postID string, paragraphIndex int, targetURL string, targetPostID string) error {
	doc, err := c.GetFullDocument("Posts", postID)
	if err != nil {
		return fmt.Errorf("fetch post for linking: %w", err)
	}

	paragraphs, ok := doc["Paragraph"].([]interface{})
	if !ok || len(paragraphs) == 0 {
		return fmt.Errorf("post %s has no paragraphs", postID)
	}
	if paragraphIndex >= len(paragraphs) {
		return fmt.Errorf("paragraph index %d out of range (has %d)", paragraphIndex, len(paragraphs))
	}

	p, ok := paragraphs[paragraphIndex].(map[string]interface{})
	if !ok {
		return fmt.Errorf("paragraph %d is not a map", paragraphIndex)
	}
	p["Hyperlink"] = targetURL
	p["BlogToLink"] = targetPostID
	paragraphs[paragraphIndex] = p

	return c.PatchDocument("Posts", postID, map[string]interface{}{
		"Paragraph": paragraphs,
	})
}

// UploadMedia uploads image bytes to the CMS Media collection and returns the document ID.
// The image is uploaded as multipart/form-data to POST /api/media.
func (c *CMSService) UploadMedia(imageData []byte, filename, altText string) (string, error) {
	token, err := c.GetToken()
	if err != nil {
		return "", err
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	// Add the file field
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return "", fmt.Errorf("cms media create form file: %w", err)
	}
	if _, err := io.Copy(part, bytes.NewReader(imageData)); err != nil {
		return "", fmt.Errorf("cms media copy: %w", err)
	}

	// Add the alt text field (required by Media collection)
	if err := writer.WriteField("alt", altText); err != nil {
		return "", fmt.Errorf("cms media alt field: %w", err)
	}

	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("cms media close writer: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/api/media", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "JWT "+token)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("cms media upload: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		var errBody map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&errBody)
		return "", fmt.Errorf("cms media upload returned %d: %v", resp.StatusCode, errBody)
	}

	var result struct {
		Doc struct {
			ID string `json:"id"`
		} `json:"doc"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("cms media decode: %w", err)
	}
	return result.Doc.ID, nil
}

// ListCategories fetches categories from the CMS.
func (c *CMSService) ListCategories(limit int) ([]map[string]interface{}, error) {
	q := url.Values{}
	q.Set("limit", fmt.Sprintf("%d", limit))
	q.Set("depth", "0")

	reqURL := fmt.Sprintf("%s/api/categories?%s", c.baseURL, q.Encode())
	resp, err := c.httpClient.Get(reqURL)
	if err != nil {
		return nil, fmt.Errorf("cms list categories: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("cms list categories returned %d", resp.StatusCode)
	}

	var result struct {
		Docs []map[string]interface{} `json:"docs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Docs, nil
}

// ListAuthors fetches authors from the CMS.
func (c *CMSService) ListAuthors(limit int) ([]map[string]interface{}, error) {
	q := url.Values{}
	q.Set("limit", fmt.Sprintf("%d", limit))
	q.Set("depth", "0")

	reqURL := fmt.Sprintf("%s/api/authors?%s", c.baseURL, q.Encode())
	resp, err := c.httpClient.Get(reqURL)
	if err != nil {
		return nil, fmt.Errorf("cms list authors: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("cms list authors returned %d", resp.StatusCode)
	}

	var result struct {
		Docs []map[string]interface{} `json:"docs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Docs, nil
}

// ListPosts fetches posts from the Posts collection with optional locale.
func (c *CMSService) ListPosts(limit int, locale string) ([]map[string]interface{}, error) {
	q := url.Values{}
	q.Set("limit", fmt.Sprintf("%d", limit))
	q.Set("depth", "0")
	if locale != "" {
		q.Set("locale", locale)
	}

	reqURL := fmt.Sprintf("%s/api/posts?%s", c.baseURL, q.Encode())
	resp, err := c.httpClient.Get(reqURL)
	if err != nil {
		return nil, fmt.Errorf("cms list posts: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("cms list posts returned %d", resp.StatusCode)
	}

	var result struct {
		Docs []map[string]interface{} `json:"docs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Docs, nil
}

// ── SEO Topics (seo-topics collection) ───────────────────────────────────────

// CreateTopic creates a new seo-topics record in CMS. Returns the created document ID.
func (c *CMSService) CreateTopic(fields map[string]interface{}) (string, error) {
	token, err := c.GetToken()
	if err != nil {
		return "", err
	}

	body, _ := json.Marshal(fields)
	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/api/seo-topics", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "JWT "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("cms create seo-topic: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		var errBody map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&errBody)
		return "", fmt.Errorf("cms create seo-topic returned %d: %v", resp.StatusCode, errBody)
	}

	var result struct {
		Doc struct {
			ID string `json:"id"`
		} `json:"doc"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("cms create seo-topic decode: %w", err)
	}
	return result.Doc.ID, nil
}

// UpdateTopic patches fields on a seo-topics record.
func (c *CMSService) UpdateTopic(topicID string, fields map[string]interface{}) error {
	return c.PatchDocument("seo-topics", topicID, fields)
}

// ListTopics fetches seo-topics records, optionally filtered by status.
func (c *CMSService) ListTopics(status string, limit int) ([]map[string]interface{}, error) {
	q := url.Values{}
	q.Set("limit", fmt.Sprintf("%d", limit))
	q.Set("depth", "0")
	q.Set("sort", "-createdAt")
	if status != "" && status != "all" {
		q.Set("where[status][equals]", status)
	}

	token, err := c.GetToken()
	if err != nil {
		return nil, err
	}

	reqURL := fmt.Sprintf("%s/api/seo-topics?%s", c.baseURL, q.Encode())
	req, err := http.NewRequest(http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "JWT "+token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cms list seo-topics: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("cms list seo-topics returned %d", resp.StatusCode)
	}

	var result struct {
		Docs []map[string]interface{} `json:"docs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Docs, nil
}

// ── SEO Suggestions (seo-suggestions collection) ─────────────────────────────

// CreateSuggestion creates a new seo-suggestions record in CMS. Returns the created document ID.
func (c *CMSService) CreateSuggestion(fields map[string]interface{}) (string, error) {
	token, err := c.GetToken()
	if err != nil {
		return "", err
	}

	body, _ := json.Marshal(fields)
	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/api/seo-suggestions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "JWT "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("cms create seo-suggestion: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		var errBody map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&errBody)
		return "", fmt.Errorf("cms create seo-suggestion returned %d: %v", resp.StatusCode, errBody)
	}

	var result struct {
		Doc struct {
			ID string `json:"id"`
		} `json:"doc"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("cms create seo-suggestion decode: %w", err)
	}
	return result.Doc.ID, nil
}

// UpdateSuggestion patches fields on a seo-suggestions record.
func (c *CMSService) UpdateSuggestion(suggestionID string, fields map[string]interface{}) error {
	return c.PatchDocument("seo-suggestions", suggestionID, fields)
}

// ListPages fetches pages from the pages collection.
func (c *CMSService) ListPages(limit int, locale string) ([]map[string]interface{}, error) {
	q := url.Values{}
	q.Set("limit", fmt.Sprintf("%d", limit))
	q.Set("depth", "0")
	if locale != "" {
		q.Set("locale", locale)
	}

	reqURL := fmt.Sprintf("%s/api/pages?%s", c.baseURL, q.Encode())
	resp, err := c.httpClient.Get(reqURL)
	if err != nil {
		return nil, fmt.Errorf("cms list pages: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("cms list pages returned %d", resp.StatusCode)
	}

	var result struct {
		Docs []map[string]interface{} `json:"docs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Docs, nil
}
