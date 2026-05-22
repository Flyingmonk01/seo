package services

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/91astro/seo-agent/internal/models"
)

const (
	pinterestAPIBase = "https://api.pinterest.com/v5"
	// authDocID is the fixed _id of the single token document in Mongo.
	authDocID = "pinterest"
)

// PinterestService publishes pins to Pinterest via the v5 API.
//
// OAuth tokens are persisted in MongoDB (collection seo_pinterest_auth) so they
// survive restarts. Access tokens last ~30 days; the long-lived refresh token
// is used to mint new ones. The refresh token is seeded once from config
// (PINTEREST_REFRESH_TOKEN) and thereafter read from — and rewritten to — Mongo.
type PinterestService struct {
	appID       string
	appSecret   string
	boardID     string
	enabled     bool
	seedRefresh string // initial refresh token from config (bootstrap only)
	authCol     *mongo.Collection
	httpClient  *http.Client

	mu          sync.Mutex
	accessToken string
	accessExpAt time.Time
}

// pinterestAuthDoc is the single persisted token record.
type pinterestAuthDoc struct {
	ID           string    `bson:"_id"`
	AccessToken  string    `bson:"access_token"`
	RefreshToken string    `bson:"refresh_token"`
	AccessExpAt  time.Time `bson:"access_exp_at"`
	UpdatedAt    time.Time `bson:"updated_at"`
}

// NewPinterestService builds the service. It is a no-op (Enabled()==false)
// unless PINTEREST_ENABLED=true and the required credentials are present.
func NewPinterestService(db *mongo.Database, enabled bool, appID, appSecret, refreshToken, boardID string) *PinterestService {
	s := &PinterestService{
		appID:       appID,
		appSecret:   appSecret,
		boardID:     boardID,
		seedRefresh: refreshToken,
		httpClient:  &http.Client{Timeout: 30 * time.Second},
	}
	if db != nil {
		s.authCol = db.Collection(models.ColPinterestAuth)
	}
	s.enabled = enabled && appID != "" && appSecret != "" && refreshToken != "" && boardID != "" && s.authCol != nil
	return s
}

// Enabled reports whether the service is configured and active.
func (s *PinterestService) Enabled() bool { return s.enabled }

// BoardID returns the configured destination board.
func (s *PinterestService) BoardID() string { return s.boardID }

// ── Pin creation ─────────────────────────────────────────────────────────────

// CreatePinInput describes a pin to publish.
type CreatePinInput struct {
	Title       string
	Description string
	Link        string // destination URL
	ImageURL    string // publicly reachable image
	AltText     string
}

// PinResult holds the created pin's identifiers.
type PinResult struct {
	ID  string
	URL string
}

// CreatePin publishes a single pin. On a 401 it refreshes the access token
// once and retries.
func (s *PinterestService) CreatePin(ctx context.Context, in CreatePinInput) (*PinResult, error) {
	if !s.enabled {
		return nil, fmt.Errorf("pinterest service is disabled")
	}

	body, _ := json.Marshal(map[string]interface{}{
		"board_id":    s.boardID,
		"title":       in.Title,
		"description": in.Description,
		"link":        in.Link,
		"alt_text":    in.AltText,
		"media_source": map[string]string{
			"source_type": "image_url",
			"url":         in.ImageURL,
		},
	})

	res, status, err := s.doPinRequest(ctx, body, false)
	if err == nil {
		return res, nil
	}
	// One retry on auth failure with a freshly refreshed token.
	if status == http.StatusUnauthorized {
		res, _, err = s.doPinRequest(ctx, body, true)
	}
	return res, err
}

func (s *PinterestService) doPinRequest(ctx context.Context, body []byte, forceRefresh bool) (*PinResult, int, error) {
	token, err := s.getAccessToken(ctx, forceRefresh)
	if err != nil {
		return nil, 0, fmt.Errorf("pinterest token: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, pinterestAPIBase+"/pins", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("pinterest create pin: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("pinterest create pin returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var pin struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &pin); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("pinterest create pin decode: %w", err)
	}
	if pin.ID == "" {
		return nil, resp.StatusCode, fmt.Errorf("pinterest create pin: empty id in response")
	}
	return &PinResult{
		ID:  pin.ID,
		URL: fmt.Sprintf("https://www.pinterest.com/pin/%s/", pin.ID),
	}, resp.StatusCode, nil
}

// ── OAuth token management ───────────────────────────────────────────────────

// getAccessToken returns a valid access token, refreshing it when needed.
func (s *PinterestService) getAccessToken(ctx context.Context, forceRefresh bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !forceRefresh && s.accessToken != "" && time.Now().Before(s.accessExpAt) {
		return s.accessToken, nil
	}

	doc := s.loadAuthDoc(ctx)

	// Reuse a still-valid persisted token (e.g. after a restart).
	if !forceRefresh && doc.AccessToken != "" && time.Now().Before(doc.AccessExpAt) {
		s.accessToken = doc.AccessToken
		s.accessExpAt = doc.AccessExpAt
		return s.accessToken, nil
	}

	refreshToken := doc.RefreshToken
	if refreshToken == "" {
		refreshToken = s.seedRefresh // first run — bootstrap from config
	}
	if refreshToken == "" {
		return "", fmt.Errorf("no refresh token available (set PINTEREST_REFRESH_TOKEN)")
	}

	access, expIn, newRefresh, err := s.refreshAccessToken(ctx, refreshToken)
	if err != nil {
		return "", err
	}
	if newRefresh != "" {
		refreshToken = newRefresh // Pinterest may rotate the refresh token
	}

	s.accessToken = access
	s.accessExpAt = time.Now().Add(time.Duration(expIn)*time.Second - 5*time.Minute)
	s.saveAuthDoc(ctx, pinterestAuthDoc{
		ID:           authDocID,
		AccessToken:  access,
		RefreshToken: refreshToken,
		AccessExpAt:  s.accessExpAt,
		UpdatedAt:    time.Now(),
	})
	return s.accessToken, nil
}

// refreshAccessToken exchanges a refresh token for a new access token.
// Returns the access token, its lifetime in seconds, and a rotated refresh
// token if Pinterest issued one (empty otherwise).
func (s *PinterestService) refreshAccessToken(ctx context.Context, refreshToken string) (string, int, string, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, pinterestAPIBase+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, "", err
	}
	basic := base64.StdEncoding.EncodeToString([]byte(s.appID + ":" + s.appSecret))
	req.Header.Set("Authorization", "Basic "+basic)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", 0, "", fmt.Errorf("pinterest token refresh: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", 0, "", fmt.Errorf("pinterest token refresh returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var result struct {
		AccessToken  string `json:"access_token"`
		ExpiresIn    int    `json:"expires_in"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", 0, "", fmt.Errorf("pinterest token refresh decode: %w", err)
	}
	if result.AccessToken == "" {
		return "", 0, "", fmt.Errorf("pinterest token refresh: empty access token")
	}
	if result.ExpiresIn <= 0 {
		result.ExpiresIn = 2592000 // default 30 days
	}
	return result.AccessToken, result.ExpiresIn, result.RefreshToken, nil
}

// loadAuthDoc reads the persisted token document; returns a zero doc if absent.
func (s *PinterestService) loadAuthDoc(ctx context.Context) pinterestAuthDoc {
	var doc pinterestAuthDoc
	if s.authCol == nil {
		return doc
	}
	_ = s.authCol.FindOne(ctx, bson.M{"_id": authDocID}).Decode(&doc)
	return doc
}

// saveAuthDoc upserts the single token document.
func (s *PinterestService) saveAuthDoc(ctx context.Context, doc pinterestAuthDoc) {
	if s.authCol == nil {
		return
	}
	_, _ = s.authCol.UpdateOne(ctx,
		bson.M{"_id": authDocID},
		bson.M{"$set": doc},
		options.Update().SetUpsert(true),
	)
}
