package workers

import (
	"context"

	"github.com/91astro/seo-agent/config"
	"github.com/91astro/seo-agent/internal/services"
)

// NewBareServer returns a Server with only the OpenAI client and config wired,
// suitable for dry-run / probe commands that exercise generation without
// touching Redis, Mongo, or the CMS. Do not use this in production paths.
func NewBareServer(cfg *config.Config) *Server {
	return &Server{
		cfg:    cfg,
		openai: services.NewOpenAIService(cfg.OpenAIAPIKey, cfg.OpenAIModel),
	}
}

// EnrichThemePublic exposes enrichTheme for use by dry-run/probe commands.
func (s *Server) EnrichThemePublic(ctx context.Context, primary string, siblings []string) (*ThemeEnrichment, error) {
	return s.enrichTheme(ctx, primary, siblings)
}

// GeneratePostPublic exposes the enriched-post generator for dry-run use.
// existingHeadings/categories are passed empty here; production daily-blog
// path uses the full Aware/Enriched signatures.
func (s *Server) GeneratePostPublic(ctx context.Context, query string, impressions int64, enrich *ThemeEnrichment) (*generatedPost, error) {
	return s.generateBlogPostEnriched(ctx, query, impressions, nil, nil, nil, enrich)
}
