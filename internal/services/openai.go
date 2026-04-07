package services

import (
	"context"
	"encoding/json"
	"fmt"

	openai "github.com/sashabaranov/go-openai"
	"github.com/91astro/seo-agent/internal/models"
)

type OpenAIService struct {
	client *openai.Client
	model  string
}

type GeneratedSEO struct {
	Current  models.SEOContent
	Proposed models.SEOContent
}

type FeatureSpec struct {
	Title           string                `json:"title"`
	Hypothesis      string                `json:"hypothesis"`
	Changes         []models.PlannedChange `json:"changes"`
	SuccessMetrics  models.SuccessMetrics  `json:"successMetrics"`
	RollbackTrigger models.RollbackTrigger `json:"rollbackTrigger"`
	FeatureFlagKey  string                `json:"featureFlagKey"`
}

func NewOpenAIService(apiKey, model string) *OpenAIService {
	return &OpenAIService{
		client: openai.NewClient(apiKey),
		model:  model,
	}
}

// Client returns the underlying OpenAI client for direct use by workers.
func (s *OpenAIService) Client() *openai.Client {
	return s.client
}

// GenerateSEOContent creates improved title + meta + FAQ for a given issue.
func (s *OpenAIService) GenerateSEOContent(ctx context.Context, issue *models.SeoIssue, current models.SEOContent) (*GeneratedSEO, error) {
	prompt := fmt.Sprintf(`You are an SEO specialist for 91Astrology, an Indian Vedic astrology platform.

Page: %s
Issue type: %s
Top query: "%s"
Current impressions: %d | Avg CTR: %.2f%% | Avg position: %.1f

Current title: "%s"
Current meta: "%s"

Generate improved SEO content. Output ONLY valid JSON matching this exact structure:
{
  "title": "max 60 chars, include primary keyword, compelling",
  "metaDescription": "max 155 chars, include keyword + CTA",
  "faqSchema": [
    {"question": "...", "answer": "..."},
    {"question": "...", "answer": "..."},
    {"question": "...", "answer": "..."}
  ]
}

Rules:
- Target Indian audience searching in English/Hindi
- Do NOT use fake superlatives
- Include the top query naturally in the title
- FAQs must answer real user intent behind the query`,
		issue.Page,
		issue.IssueType,
		issue.TopQuery,
		issue.Metrics.TotalImpressions,
		issue.Metrics.AvgCTR,
		issue.Metrics.AvgPosition,
		current.Title,
		current.MetaDescription,
	)

	resp, err := s.client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: s.model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: "You are an SEO specialist. Output only valid JSON, no markdown."},
			{Role: openai.ChatMessageRoleUser, Content: prompt},
		},
		Temperature: 0.7,
	})
	if err != nil {
		return nil, fmt.Errorf("openai content generation: %w", err)
	}

	var proposed models.SEOContent
	if err := json.Unmarshal([]byte(resp.Choices[0].Message.Content), &proposed); err != nil {
		return nil, fmt.Errorf("parse openai response: %w", err)
	}

	return &GeneratedSEO{Current: current, Proposed: proposed}, nil
}

// GenerateFeatureSpec creates a feature implementation plan from research signals.
func (s *OpenAIService) GenerateFeatureSpec(ctx context.Context, page string, signals []models.ResearchSignal, learnings []models.SeoLearning) (*FeatureSpec, error) {
	signalsJSON, _ := json.MarshalIndent(signals, "", "  ")
	learningsJSON, _ := json.MarshalIndent(learnings, "", "  ")

	prompt := fmt.Sprintf(`You are a senior engineer at 91Astrology.
Stack: Next.js 13 App Router, NestJS, MongoDB, Tailwind CSS, Mantine UI.

Page: %s
Research signals:
%s

Past successful patterns (use these to inform your plan):
%s

Generate a conservative, additive feature spec. Output ONLY valid JSON:
{
  "title": "short feature name (max 50 chars)",
  "hypothesis": "if we add X, metric Y will improve because...",
  "changes": [
    {
      "type": "new_component|new_api_endpoint|data_change",
      "file": "relative path from repo root",
      "description": "what to build, 1-2 sentences"
    }
  ],
  "successMetrics": {
    "bounceRateDelta": -10,
    "sessionDelta": 20,
    "conversionDelta": 0.5
  },
  "rollbackTrigger": {
    "bounceRateDelta": 5,
    "errorRateDelta": 1
  },
  "featureFlagKey": "seo_<page_slug>_<feature_slug>"
}

Rules:
- Prefer additive changes (add component) over modifications
- Maximum 3 file changes per feature
- Each change must be independently reversible via feature flag
- Do NOT suggest layout restructuring or design changes`,
		page,
		string(signalsJSON),
		string(learningsJSON),
	)

	resp, err := s.client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: s.model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: "You are a senior engineer. Output only valid JSON, no markdown."},
			{Role: openai.ChatMessageRoleUser, Content: prompt},
		},
		Temperature: 0.5,
	})
	if err != nil {
		return nil, fmt.Errorf("openai feature spec: %w", err)
	}

	var spec FeatureSpec
	if err := json.Unmarshal([]byte(resp.Choices[0].Message.Content), &spec); err != nil {
		return nil, fmt.Errorf("parse feature spec: %w", err)
	}

	return &spec, nil
}

// GenerateCode writes or modifies a file for a planned change.
// existingContent is the current file contents fetched from the repo ("" for new files).
func (s *OpenAIService) GenerateCode(ctx context.Context, change models.PlannedChange, plan *FeatureSpec, existingContent string) (string, error) {
	var fileContext string
	if existingContent != "" {
		// Truncate very large files to avoid token limits
		if len(existingContent) > 8000 {
			existingContent = existingContent[:8000] + "\n\n// ... (file truncated for context)"
		}
		fileContext = fmt.Sprintf(`
EXISTING FILE CONTENT (modify this — do not rewrite from scratch):
` + "```" + `
%s
` + "```", existingContent)
	} else {
		fileContext = "This is a NEW file — create it from scratch."
	}

	prompt := fmt.Sprintf(`You are a senior engineer at 91Astrology.
Stack: Next.js 13 App Router, NestJS, MongoDB, Tailwind CSS, Mantine UI.

Feature: %s
Hypothesis: %s
File: %s
Change required: %s
Feature flag key: %s

%s

Instructions:
- If modifying an existing file: make the minimal change needed. Keep all existing code intact.
- If creating a new file: follow the exact same patterns as the existing codebase.
- Use TypeScript for .ts/.tsx files, JavaScript for .js/.jsx files.
- Wrap any new UI behind: if (featureFlags['%s']) { ... }
- Output ONLY the complete final file contents. No explanation. No markdown code fences.`,
		plan.Title,
		plan.Hypothesis,
		change.File,
		change.Description,
		plan.FeatureFlagKey,
		fileContext,
		plan.FeatureFlagKey,
	)

	resp, err := s.client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: s.model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: "You are a senior engineer. Output only raw code, no markdown."},
			{Role: openai.ChatMessageRoleUser, Content: prompt},
		},
		Temperature: 0.2,
	})
	if err != nil {
		return "", fmt.Errorf("openai code generation: %w", err)
	}

	return resp.Choices[0].Message.Content, nil
}
