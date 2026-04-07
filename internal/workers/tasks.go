package workers

// Task type constants — used by both scheduler (enqueue) and server (route to handler)
const (
	// Pipeline 1: SEO Metadata
	TaskIngest          = "seo:ingest"
	TaskAnalyze         = "seo:analyze"
	TaskGenerateContent = "seo:generate_content"
	TaskTrackImpact     = "seo:track_impact"
	TaskReport          = "seo:report"

	// Pipeline 2: Feature Development
	TaskResearch       = "seo:research"
	TaskPlanFeature    = "seo:plan_feature"
	TaskCodeFeature    = "seo:code_feature"
	TaskMonitorFeature = "seo:monitor_feature"

	// Pipeline 3: Page & Widget Optimization (CMS-driven, no blog modifications)
	TaskCreateContent  = "seo:create_content"
	TaskGenerateFAQ    = "seo:generate_faq"
	TaskRefreshContent = "seo:refresh_content"
	TaskInternalLink   = "seo:internal_link"

	// Pipeline 4: Daily Blog Content Generation
	TaskDailyBlogCreate  = "seo:daily_blog_create"
	TaskRegenerateBlog   = "seo:regenerate_blog"
)
