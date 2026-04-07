package services

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/91astro/seo-agent/internal/models"
)

type ExecuteService struct {
	nextRevalidateURL string
	revalidateSecret  string
	cms               *CMSService
	httpClient        *http.Client
}

func NewExecuteService(nextRevalidateURL, revalidateSecret string, cms *CMSService) *ExecuteService {
	return &ExecuteService{
		nextRevalidateURL: nextRevalidateURL,
		revalidateSecret:  revalidateSecret,
		cms:               cms,
		httpClient:        &http.Client{},
	}
}

// IsConfigured returns true when CMS is wired up.
func (e *ExecuteService) IsConfigured() bool {
	return e.cms != nil && e.cms.IsConfigured()
}

// ApplySEOChange updates the page in Payload CMS and busts ISR cache.
func (e *ExecuteService) ApplySEOChange(suggestion *models.SeoSuggestion) error {
	if err := e.updatePayloadCMS(suggestion); err != nil {
		return fmt.Errorf("cms update: %w", err)
	}

	if err := e.revalidatePage(suggestion.Page); err != nil {
		log.Printf("WARN: revalidate failed for %s: %v (non-fatal)", suggestion.Page, err)
	}
	return nil
}

// updatePayloadCMS resolves the CMS target and patches it.
func (e *ExecuteService) updatePayloadCMS(suggestion *models.SeoSuggestion) error {
	if e.cms == nil || !e.cms.IsConfigured() {
		return fmt.Errorf("CMS service not configured")
	}

	target, err := e.cms.ResolveTarget(suggestion.Page)
	if err != nil {
		// Fallback: use stored CMSPageID if resolution failed
		if suggestion.CMSPageID != "" {
			return e.cms.UpdatePageMeta(suggestion.CMSPageID, suggestion.Proposed.Title, suggestion.Proposed.MetaDescription)
		}
		return fmt.Errorf("CMS resolve: %w", err)
	}

	switch target.Collection {
	case "Posts":
		return e.cms.UpdatePostMeta(target.DocID, suggestion.Proposed.Title, suggestion.Proposed.MetaDescription)
	default:
		return e.cms.UpdatePageMeta(target.DocID, suggestion.Proposed.Title, suggestion.Proposed.MetaDescription)
	}
}

// Rollback restores previous SEO content for a page via CMS.
func (e *ExecuteService) Rollback(change *models.SeoChange) error {
	target, err := e.cms.ResolveTarget(change.Page)
	if err != nil {
		return fmt.Errorf("rollback resolve: %w", err)
	}

	switch target.Collection {
	case "Posts":
		err = e.cms.UpdatePostMeta(target.DocID, change.RollbackData.Title, change.RollbackData.MetaDescription)
	default:
		err = e.cms.UpdatePageMeta(target.DocID, change.RollbackData.Title, change.RollbackData.MetaDescription)
	}
	if err != nil {
		return fmt.Errorf("rollback patch: %w", err)
	}

	e.revalidatePage(change.Page)
	return nil
}

func (e *ExecuteService) revalidatePage(path string) error {
	if e.nextRevalidateURL == "" || e.revalidateSecret == "" ||
		e.revalidateSecret == "generate-a-strong-random-secret" {
		return nil
	}

	payload := map[string]string{
		"path":   path,
		"secret": e.revalidateSecret,
	}
	body, _ := json.Marshal(payload)

	resp, err := e.httpClient.Post(
		e.nextRevalidateURL+"/api/revalidate",
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("revalidate returned %d", resp.StatusCode)
	}
	return nil
}

// helper used in gsc.go
func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}
