package models

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// ── Collections ──────────────────────────────────────────────────────────────

const (
	ColRawData    = "seo_raw_data"
	ColPageStats  = "seo_page_stats"
	ColIssues     = "seo_issues"
	ColSuggestions = "seo_suggestions"
	ColChanges    = "seo_changes"
	ColResults    = "seo_results"
	ColFeatures   = "seo_features"
	ColLearnings  = "seo_learnings"
	ColFlags      = "seo_feature_flags"
	ColBlogTopics = "seo_blog_topics"
)

// ── Issue / Suggestion status ─────────────────────────────────────────────────

type IssueStatus string
type SuggestionStatus string
type FeatureStatus string
type IssueType string
type WidgetType string
type OutcomeType string

const (
	IssuePendingGeneration IssueStatus = "pending_generation"
	IssuePendingApproval   IssueStatus = "pending_approval"
	IssueApproved          IssueStatus = "approved"
	IssueRejected          IssueStatus = "rejected"
	IssueLive              IssueStatus = "live"

	SuggestionPending  SuggestionStatus = "pending"
	SuggestionApproved SuggestionStatus = "approved"  // human approved, push pending/failed
	SuggestionRejected SuggestionStatus = "rejected"
	SuggestionLive     SuggestionStatus = "live"       // successfully pushed to backend

	FeatureResearching FeatureStatus = "researching"
	FeaturePlanning    FeatureStatus = "planning"
	FeatureCoding      FeatureStatus = "coding"
	FeaturePROpen      FeatureStatus = "pr_open"
	FeatureMonitoring  FeatureStatus = "monitoring"
	FeatureLive        FeatureStatus = "live"
	FeatureRolledBack  FeatureStatus = "rolled_back"

	IssueTypeLowCTR             IssueType = "low_ctr"
	IssueTypeRankingOpportunity IssueType = "ranking_opportunity"
	IssueTypeScaling            IssueType = "scaling"
	IssueTypeMobileGap          IssueType = "mobile_gap"          // good on desktop, bad on mobile
	IssueTypeDecliningCTR       IssueType = "declining_ctr"       // CTR dropping vs previous period
	IssueTypeContentGap         IssueType = "content_gap"         // high impressions, no good page
	IssueTypeCannibalization    IssueType = "cannibalization"      // same query ranking on multiple pages

	WidgetTypeFAQ        WidgetType = "faq"
	WidgetTypeRating     WidgetType = "rating"
	WidgetTypeRelated    WidgetType = "related"
	WidgetTypeBreadcrumb WidgetType = "breadcrumb"
	WidgetTypeCalcCTA    WidgetType = "calculator_cta"

	OutcomeSuccess OutcomeType = "success"
	OutcomeFailure OutcomeType = "failure"
)

// ── SeoRawData ────────────────────────────────────────────────────────────────

type SeoRawData struct {
	ID          primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	Page        string             `bson:"page" json:"page"`
	Query       string             `bson:"query" json:"query"`
	Clicks      int64              `bson:"clicks" json:"clicks"`
	Impressions int64              `bson:"impressions" json:"impressions"`
	CTR         float64            `bson:"ctr" json:"ctr"`
	Position    float64            `bson:"position" json:"position"`
	Date        string             `bson:"date" json:"date"` // YYYY-MM-DD
	Device      string             `bson:"device" json:"device"`           // DESKTOP, MOBILE, TABLET
	Country     string             `bson:"country" json:"country"`         // 3-letter country code
	Intent      string             `bson:"intent" json:"intent"`           // informational, transactional, navigational, local
	Cluster     string             `bson:"cluster" json:"cluster"`         // normalized query cluster key
	PosBucket   string             `bson:"pos_bucket" json:"posBucket"`    // top_3, first_page, striking, deep
	Locale      string             `bson:"locale" json:"locale"`
	CreatedAt   time.Time          `bson:"created_at" json:"createdAt"`
}

// ── SeoPageStats (aggregated page-level metrics) ──────────────────────────────

type SeoPageStats struct {
	ID               primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	Page             string             `bson:"page" json:"page"`
	Date             string             `bson:"date" json:"date"`
	TotalClicks      int64              `bson:"total_clicks" json:"totalClicks"`
	TotalImpressions int64              `bson:"total_impressions" json:"totalImpressions"`
	AvgCTR           float64            `bson:"avg_ctr" json:"avgCTR"`
	AvgPosition      float64            `bson:"avg_position" json:"avgPosition"`
	TopQuery         string             `bson:"top_query" json:"topQuery"`
	QueryCount       int                `bson:"query_count" json:"queryCount"`
	MobileShare      float64            `bson:"mobile_share" json:"mobileShare"`       // % of impressions from mobile
	TopCountry       string             `bson:"top_country" json:"topCountry"`
	DominantIntent   string             `bson:"dominant_intent" json:"dominantIntent"` // most common intent for this page
	// Deltas (vs previous period)
	ClicksDelta      int64              `bson:"clicks_delta" json:"clicksDelta"`
	CTRDelta         float64            `bson:"ctr_delta" json:"ctrDelta"`
	PositionDelta    float64            `bson:"position_delta" json:"positionDelta"`
	CreatedAt        time.Time          `bson:"created_at" json:"createdAt"`
}

// ── SeoIssue ──────────────────────────────────────────────────────────────────

type PageMetrics struct {
	AvgCTR          float64 `bson:"avg_ctr" json:"avgCTR"`
	AvgPosition     float64 `bson:"avg_position" json:"avgPosition"`
	TotalImpressions int64  `bson:"total_impressions" json:"totalImpressions"`
	TotalClicks     int64   `bson:"total_clicks" json:"totalClicks"`
}

type SeoIssue struct {
	ID            primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	Page          string             `bson:"page" json:"page"`
	TopQuery      string             `bson:"top_query" json:"topQuery"`
	IssueType     IssueType          `bson:"issue_type" json:"issueType"`
	PriorityScore float64            `bson:"priority_score" json:"priorityScore"`
	Metrics       PageMetrics        `bson:"metrics" json:"metrics"`
	Status        IssueStatus        `bson:"status" json:"status"`
	// New intelligence fields
	Intent        string             `bson:"intent,omitempty" json:"intent,omitempty"`
	Device        string             `bson:"device,omitempty" json:"device,omitempty"`           // which device has worst performance
	MobileShare   float64            `bson:"mobile_share,omitempty" json:"mobileShare,omitempty"` // % mobile traffic
	PosBucket     string             `bson:"pos_bucket,omitempty" json:"posBucket,omitempty"`
	Cluster       string             `bson:"cluster,omitempty" json:"cluster,omitempty"`
	CTRDelta      float64            `bson:"ctr_delta,omitempty" json:"ctrDelta,omitempty"`       // trend vs previous period
	DetectedAt    time.Time          `bson:"detected_at" json:"detectedAt"`
}

// ── SeoSuggestion ─────────────────────────────────────────────────────────────

type SEOContent struct {
	Title           string      `bson:"title" json:"title"`
	MetaDescription string      `bson:"meta_description" json:"metaDescription"`
	FAQSchema       []FAQItem   `bson:"faq_schema,omitempty" json:"faqSchema,omitempty"`
}

type FAQItem struct {
	Question string `bson:"question" json:"question"`
	Answer   string `bson:"answer" json:"answer"`
}

type WidgetData struct {
	Type   WidgetType  `bson:"type" json:"type"`
	FAQs   []FAQItem   `bson:"faqs,omitempty" json:"faqs,omitempty"`
	Links  []LinkItem  `bson:"links,omitempty" json:"links,omitempty"`
	Rating *RatingData `bson:"rating,omitempty" json:"rating,omitempty"`
}

type LinkItem struct {
	Title       string `bson:"title" json:"title"`
	Href        string `bson:"href" json:"href"`
	Description string `bson:"description" json:"description"`
}

type RatingData struct {
	Score float64 `bson:"score" json:"score"`
	Count int64   `bson:"count" json:"count"`
	Label string  `bson:"label" json:"label"`
}

// PageSource identifies where a page's metadata is stored.
// "cms"     → Payload CMS (cms1.91astrology.com) — update via PATCH /api/pages/:id
// "api"     → 91astro-api NestJS service          — update via internal PATCH endpoint
// "website" → Next.js static data file            — update via Bitbucket PR
// ""        → unknown, executor will try CMS first then API
type PageSource = string

const (
	PageSourceCMS PageSource = "cms"
)

type SeoSuggestion struct {
	ID           primitive.ObjectID  `bson:"_id,omitempty" json:"id"`
	IssueID      primitive.ObjectID  `bson:"issue_id" json:"issueId"`
	Page         string              `bson:"page" json:"page"`
	Locale       string              `bson:"locale" json:"locale"`
	PageSource   PageSource          `bson:"page_source,omitempty" json:"pageSource,omitempty"`
	CMSPageID    string              `bson:"cms_page_id,omitempty" json:"cmsPageId,omitempty"`
	Current      SEOContent          `bson:"current" json:"current"`
	Proposed     SEOContent          `bson:"proposed" json:"proposed"`
	Widget       *WidgetData         `bson:"widget,omitempty" json:"widget,omitempty"`
	GeneratedBy  string              `bson:"generated_by" json:"generatedBy"`
	Status       SuggestionStatus    `bson:"status" json:"status"`
	ReviewedBy   *primitive.ObjectID `bson:"reviewed_by,omitempty" json:"reviewedBy,omitempty"`
	ReviewedAt   *time.Time          `bson:"reviewed_at,omitempty" json:"reviewedAt,omitempty"`
	ApprovalNote string              `bson:"approval_note,omitempty" json:"approvalNote,omitempty"`
	CreatedAt    time.Time           `bson:"created_at" json:"createdAt"`
}

// ── SeoChange ─────────────────────────────────────────────────────────────────

type SeoChange struct {
	ID              primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	SuggestionID    primitive.ObjectID `bson:"suggestion_id" json:"suggestionId"`
	Page            string             `bson:"page" json:"page"`
	AppliedAt       time.Time          `bson:"applied_at" json:"appliedAt"`
	AppliedBy       primitive.ObjectID `bson:"applied_by" json:"appliedBy"`
	BaselineMetrics PageMetrics        `bson:"baseline_metrics" json:"baselineMetrics"`
	RollbackData    SEOContent         `bson:"rollback_data" json:"rollbackData"`
}

// ── SeoResult ─────────────────────────────────────────────────────────────────

type MetricSnapshot struct {
	CTR         float64 `bson:"ctr" json:"ctr"`
	Position    float64 `bson:"position" json:"position"`
	Clicks      int64   `bson:"clicks" json:"clicks"`
	Impressions int64   `bson:"impressions" json:"impressions"`
}

type MetricDelta struct {
	CTRDelta      float64 `bson:"ctr_delta" json:"ctrDelta"`
	PositionDelta float64 `bson:"position_delta" json:"positionDelta"`
	ClicksDelta   int64   `bson:"clicks_delta" json:"clicksDelta"`
}

type SeoResult struct {
	ID          primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	ChangeID    primitive.ObjectID `bson:"change_id" json:"changeId"`
	Page        string             `bson:"page" json:"page"`
	Window      int                `bson:"window" json:"window"` // 7, 14, 30
	Before      MetricSnapshot     `bson:"before" json:"before"`
	After       MetricSnapshot     `bson:"after" json:"after"`
	Delta       MetricDelta        `bson:"delta" json:"delta"`
	MeasuredAt  time.Time          `bson:"measured_at" json:"measuredAt"`
}

// ── SeoFeature (Pipeline 2 - autonomous feature dev) ─────────────────────────

type ResearchSignal struct {
	Type     string `bson:"type" json:"type"`
	Evidence string `bson:"evidence" json:"evidence"`
	Suggest  string `bson:"suggestion" json:"suggestion"`
}

type FeaturePlan struct {
	Title          string          `bson:"title" json:"title"`
	Hypothesis     string          `bson:"hypothesis" json:"hypothesis"`
	Changes        []PlannedChange `bson:"changes" json:"changes"`
	SuccessMetrics SuccessMetrics  `bson:"success_metrics" json:"successMetrics"`
	RollbackTrigger RollbackTrigger `bson:"rollback_trigger" json:"rollbackTrigger"`
	FeatureFlagKey string          `bson:"feature_flag_key" json:"featureFlagKey"`
}

type PlannedChange struct {
	Type        string `bson:"type" json:"type"` // new_component, new_api_endpoint, data_change
	File        string `bson:"file" json:"file"`
	Description string `bson:"description" json:"description"`
}

type SuccessMetrics struct {
	BounceRateDelta    float64 `bson:"bounce_rate_delta" json:"bounceRateDelta"`
	SessionDelta       float64 `bson:"session_delta" json:"sessionDelta"`
	ConversionDelta    float64 `bson:"conversion_delta" json:"conversionDelta"`
}

type RollbackTrigger struct {
	BounceRateDelta float64 `bson:"bounce_rate_delta" json:"bounceRateDelta"`
	ErrorRateDelta  float64 `bson:"error_rate_delta" json:"errorRateDelta"`
}

type MonitorResult struct {
	Window          int     `bson:"window" json:"window"`
	BounceRateDelta float64 `bson:"bounce_rate_delta" json:"bounceRateDelta"`
	SessionDelta    float64 `bson:"session_delta" json:"sessionDelta"`
	ConversionDelta float64 `bson:"conversion_delta" json:"conversionDelta"`
	ErrorRateDelta  float64 `bson:"error_rate_delta" json:"errorRateDelta"`
	MeasuredAt      time.Time `bson:"measured_at" json:"measuredAt"`
}

type SeoFeature struct {
	ID              primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	Page            string             `bson:"page" json:"page"`
	Signals         []ResearchSignal   `bson:"signals" json:"signals"`
	Plan            *FeaturePlan       `bson:"plan,omitempty" json:"plan,omitempty"`
	PRUrl           string             `bson:"pr_url,omitempty" json:"prUrl,omitempty"`
	PRNumber        int                `bson:"pr_number,omitempty" json:"prNumber,omitempty"`
	BranchName      string             `bson:"branch_name,omitempty" json:"branchName,omitempty"`
	Status          FeatureStatus      `bson:"status" json:"status"`
	MonitorResults  []MonitorResult    `bson:"monitor_results,omitempty" json:"monitorResults,omitempty"`
	RolledBackAt    *time.Time         `bson:"rolled_back_at,omitempty" json:"rolledBackAt,omitempty"`
	PromotedAt      *time.Time         `bson:"promoted_at,omitempty" json:"promotedAt,omitempty"`
	CreatedAt       time.Time          `bson:"created_at" json:"createdAt"`
}

// ── SeoLearning (self-improvement store) ─────────────────────────────────────

type LearningConditions struct {
	AvgBounceRate float64            `bson:"avg_bounce_rate" json:"avgBounceRate"`
	TopQueryType  string             `bson:"top_query_type" json:"topQueryType"`
	DeviceSplit   map[string]float64 `bson:"device_split" json:"deviceSplit"`
	PageType      string             `bson:"page_type" json:"pageType"`
}

type SeoLearning struct {
	ID          primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	FeatureType string             `bson:"feature_type" json:"featureType"`
	PageType    string             `bson:"page_type" json:"pageType"`
	Hypothesis  string             `bson:"hypothesis" json:"hypothesis"`
	Outcome     OutcomeType        `bson:"outcome" json:"outcome"`
	Metrics     MonitorResult      `bson:"metrics" json:"metrics"`
	Conditions  LearningConditions `bson:"conditions" json:"conditions"`
	LearnedAt   time.Time          `bson:"learned_at" json:"learnedAt"`
}

// ── SeoBlogTopic (tracks queries used for blog generation to prevent repeats) ─

type SeoBlogTopic struct {
	ID           primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	Query        string             `bson:"query" json:"query"`
	ClusterKey   string             `bson:"cluster_key" json:"clusterKey"`
	PostID       string             `bson:"post_id" json:"postId"`
	Heading      string             `bson:"heading" json:"heading"`
	Status        string             `bson:"status" json:"status"`                                  // pending, approved, rejected, regenerating
	RejectReason  string             `bson:"reject_reason,omitempty" json:"rejectReason,omitempty"`
	CreatedAt     time.Time          `bson:"created_at" json:"createdAt"`
	ApprovedAt    *time.Time         `bson:"approved_at,omitempty" json:"approvedAt,omitempty"`
	RejectedAt    *time.Time         `bson:"rejected_at,omitempty" json:"rejectedAt,omitempty"`
	RegeneratedAt *time.Time         `bson:"regenerated_at,omitempty" json:"regeneratedAt,omitempty"`
}

// ── CodeTask (OpenClaw webhook queue) ────────────────────────────────────────

const ColCodeTasks = "seo_code_tasks"

type CodeTaskStatus = string

const (
	CodeTaskPending    CodeTaskStatus = "pending"    // waiting for OpenClaw to pick up
	CodeTaskInProgress CodeTaskStatus = "in_progress" // OpenClaw is working on it
	CodeTaskDone       CodeTaskStatus = "done"        // PR opened successfully
	CodeTaskFailed     CodeTaskStatus = "failed"      // OpenClaw reported failure
)

// CodeTaskFile describes a single file change OpenClaw needs to make.
type CodeTaskFile struct {
	Repo        string `bson:"repo" json:"repo"`               // "website" or "api"
	FilePath    string `bson:"file_path" json:"filePath"`       // relative path in repo
	Description string `bson:"description" json:"description"` // what to change + why
}

// CodeTask is queued by the planner and picked up by OpenClaw.
type CodeTask struct {
	ID          primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	FeatureID   primitive.ObjectID `bson:"feature_id" json:"featureId"`
	Title       string             `bson:"title" json:"title"`
	Hypothesis  string             `bson:"hypothesis" json:"hypothesis"`
	BranchName  string             `bson:"branch_name" json:"branchName"`
	Files       []CodeTaskFile     `bson:"files" json:"files"`
	Status      CodeTaskStatus     `bson:"status" json:"status"`
	PRUrl       string             `bson:"pr_url,omitempty" json:"prUrl,omitempty"`
	ErrorMsg    string             `bson:"error_msg,omitempty" json:"errorMsg,omitempty"`
	CreatedAt   time.Time          `bson:"created_at" json:"createdAt"`
	PickedUpAt  *time.Time         `bson:"picked_up_at,omitempty" json:"pickedUpAt,omitempty"`
	CompletedAt *time.Time         `bson:"completed_at,omitempty" json:"completedAt,omitempty"`
}

// ── FeatureFlag ───────────────────────────────────────────────────────────────

type FeatureFlag struct {
	ID              primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	Key             string             `bson:"key" json:"key"`
	Enabled         bool               `bson:"enabled" json:"enabled"`
	RolloutPercent  int                `bson:"rollout_percent" json:"rolloutPercent"`
	TargetUserIDs   []string           `bson:"target_user_ids,omitempty" json:"targetUserIds,omitempty"`
	CreatedBy       string             `bson:"created_by" json:"createdBy"`
	CreatedAt       time.Time          `bson:"created_at" json:"createdAt"`
	PromotedAt      *time.Time         `bson:"promoted_at,omitempty" json:"promotedAt,omitempty"`
	RolledBackAt    *time.Time         `bson:"rolled_back_at,omitempty" json:"rolledBackAt,omitempty"`
}
