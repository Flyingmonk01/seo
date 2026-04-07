package workers

import (
	"context"
	"fmt"
	"log"
	"math"
	"time"

	"github.com/91astro/seo-agent/internal/models"
	"github.com/91astro/seo-agent/internal/services"
	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// expectedCTR returns benchmark CTR% for a given average position.
func expectedCTR(position float64) float64 {
	benchmarks := map[float64]float64{
		1: 28, 2: 15, 3: 12, 4: 10, 5: 9,
		6: 6, 7: 5, 8: 4, 9: 3, 10: 2.5,
		15: 1, 20: 0.5,
	}
	closest := 20.0
	for pos := range benchmarks {
		if math.Abs(pos-position) < math.Abs(closest-position) {
			closest = pos
		}
	}
	return benchmarks[closest]
}

func priorityScore(impressions int64, ctr, position float64) float64 {
	ctrGap := math.Max(0, expectedCTR(position)-ctr)
	posOpportunity := math.Max(0, (15-position)/10)
	return float64(impressions)*0.4 + ctrGap*0.3 + posOpportunity*0.3
}

func (s *Server) handleAnalyze(ctx context.Context, task *asynq.Task) error {
	log.Println("[analyst] ─────────────────────────────────────────")
	log.Println("[analyst] Analyzing GSC data for SEO issues...")
	log.Println("[analyst] Scoring formula: (impressions×0.4) + (ctrGap×0.3) + (positionOpportunity×0.3)")
	log.Println("[analyst] Issue thresholds:")
	log.Println("[analyst]   low_ctr:             impressions>500, CTR<2%, position≤15")
	log.Println("[analyst]   ranking_opportunity: position 5–15, impressions>200")
	log.Println("[analyst]   scaling:             CTR>5%, impressions<100")

	col := s.db.Collection(models.ColRawData)

	// Aggregate last 30 days per page+query
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: bson.D{
				{Key: "page", Value: "$page"},
				{Key: "query", Value: "$query"},
			}},
			{Key: "avgCTR", Value: bson.D{{Key: "$avg", Value: "$ctr"}}},
			{Key: "avgPosition", Value: bson.D{{Key: "$avg", Value: "$position"}}},
			{Key: "totalImpressions", Value: bson.D{{Key: "$sum", Value: "$impressions"}}},
			{Key: "totalClicks", Value: bson.D{{Key: "$sum", Value: "$clicks"}}},
		}}},
	}

	cursor, err := col.Aggregate(ctx, pipeline)
	if err != nil {
		return err
	}
	defer cursor.Close(ctx)

	var results []struct {
		ID               struct{ Page, Query string } `bson:"_id"`
		AvgCTR           float64                      `bson:"avgCTR"`
		AvgPosition      float64                      `bson:"avgPosition"`
		TotalImpressions int64                        `bson:"totalImpressions"`
		TotalClicks      int64                        `bson:"totalClicks"`
	}
	if err := cursor.All(ctx, &results); err != nil {
		return err
	}

	issueCol := s.db.Collection(models.ColIssues)
	detected := 0
	skipped := 0

	counters := map[models.IssueType]int{}

	log.Printf("[analyst] Evaluating %d page+query combinations...", len(results))

	for _, r := range results {
		var issueType models.IssueType
		var reason string

		expectedCTRVal := expectedCTR(r.AvgPosition)

		switch {
		case r.TotalImpressions > 500 && r.AvgCTR < 2 && r.AvgPosition <= 15:
			issueType = models.IssueTypeLowCTR
			reason = fmt.Sprintf("high volume (impr=%d) but low CTR=%.2f%% vs expected=%.1f%% at pos=%.1f",
				r.TotalImpressions, r.AvgCTR, expectedCTRVal, r.AvgPosition)
		case r.AvgPosition >= 5 && r.AvgPosition <= 15 && r.TotalImpressions > 200:
			issueType = models.IssueTypeRankingOpportunity
			reason = fmt.Sprintf("ranking at pos=%.1f with %d impr — push to top 5 could 3x clicks",
				r.AvgPosition, r.TotalImpressions)
		case r.AvgCTR > 5 && r.TotalImpressions < 100:
			issueType = models.IssueTypeScaling
			reason = fmt.Sprintf("great CTR=%.2f%% but only %d impressions — under-indexed keyword",
				r.AvgCTR, r.TotalImpressions)
		default:
			skipped++
			continue
		}

		score := priorityScore(r.TotalImpressions, r.AvgCTR, r.AvgPosition)

		log.Printf("[analyst] ISSUE %-22s | score=%.1f | query=%-40s | %s",
			issueType, score, fmt.Sprintf("%q", r.ID.Query), reason)

		issue := models.SeoIssue{
			Page:          r.ID.Page,
			TopQuery:      r.ID.Query,
			IssueType:     issueType,
			PriorityScore: score,
			Metrics: models.PageMetrics{
				AvgCTR:           r.AvgCTR,
				AvgPosition:      r.AvgPosition,
				TotalImpressions: r.TotalImpressions,
				TotalClicks:      r.TotalClicks,
			},
			Status:     models.IssuePendingGeneration,
			DetectedAt: time.Now(),
		}

		// Upsert — don't duplicate open issues for same page
		filter := bson.D{
			{Key: "page", Value: r.ID.Page},
			{Key: "status", Value: bson.D{{Key: "$in", Value: []models.IssueStatus{
				models.IssuePendingGeneration, models.IssuePendingApproval,
			}}}},
		}
		update := bson.D{{Key: "$setOnInsert", Value: issue}}
		opts := optionsUpsert()
		if _, err := issueCol.UpdateOne(ctx, filter, update, opts); err != nil {
			log.Printf("WARN: upsert issue for %s: %v", r.ID.Page, err)
		} else {
			counters[issueType]++
			detected++
		}
	}

	// ── New: detect mobile gap issues ─────────────────────────────────────
	mobileGaps, err := s.detectMobileGaps(ctx)
	if err != nil {
		log.Printf("[analyst] WARN: mobile gap detection: %v", err)
	} else {
		for _, mg := range mobileGaps {
			issueCol.UpdateOne(ctx, bson.D{
				{Key: "page", Value: mg.Page},
				{Key: "issue_type", Value: models.IssueTypeMobileGap},
				{Key: "status", Value: bson.D{{Key: "$in", Value: []models.IssueStatus{
					models.IssuePendingGeneration, models.IssuePendingApproval,
				}}}},
			}, bson.D{{Key: "$setOnInsert", Value: mg}}, optionsUpsert())
			counters[models.IssueTypeMobileGap]++
			detected++
		}
	}

	// ── New: detect cannibalization ───────────────────────────────────────
	cannibalized, err := s.detectCannibalization(ctx)
	if err != nil {
		log.Printf("[analyst] WARN: cannibalization detection: %v", err)
	} else {
		for _, c := range cannibalized {
			issueCol.UpdateOne(ctx, bson.D{
				{Key: "page", Value: c.Page},
				{Key: "issue_type", Value: models.IssueTypeCannibalization},
				{Key: "status", Value: bson.D{{Key: "$in", Value: []models.IssueStatus{
					models.IssuePendingGeneration, models.IssuePendingApproval,
				}}}},
			}, bson.D{{Key: "$setOnInsert", Value: c}}, optionsUpsert())
			counters[models.IssueTypeCannibalization]++
			detected++
		}
	}

	// ── Aggregate page-level stats ────────────────────────────────────────
	date := time.Now().AddDate(0, 0, -4).Format("2006-01-02")
	if err := s.aggregatePageStats(ctx, date); err != nil {
		log.Printf("[analyst] WARN: page stats aggregation: %v", err)
	}

	log.Println("[analyst] ── Summary ──────────────────────────────")
	log.Printf("[analyst]   low_ctr:             %d issues", counters[models.IssueTypeLowCTR])
	log.Printf("[analyst]   ranking_opportunity: %d issues", counters[models.IssueTypeRankingOpportunity])
	log.Printf("[analyst]   scaling:             %d issues", counters[models.IssueTypeScaling])
	log.Printf("[analyst]   mobile_gap:          %d issues", counters[models.IssueTypeMobileGap])
	log.Printf("[analyst]   cannibalization:     %d issues", counters[models.IssueTypeCannibalization])
	log.Printf("[analyst]   skipped (no issue):  %d rows", skipped)
	log.Printf("[analyst]   total new issues:    %d", detected)

	// Raw data has been fully processed — drop it to keep storage flat.
	// seo_issues and seo_page_stats hold everything needed for downstream tasks.
	if delRes, err := col.DeleteMany(ctx, bson.D{}); err != nil {
		log.Printf("[analyst] WARN: could not clear raw data: %v", err)
	} else {
		log.Printf("[analyst]   raw data cleared: %d rows deleted", delRes.DeletedCount)
	}

	log.Println("[analyst] ─────────────────────────────────────────")
	return nil
}

// detectMobileGaps finds pages that perform well on desktop but poorly on mobile.
func (s *Server) detectMobileGaps(ctx context.Context) ([]models.SeoIssue, error) {
	rawCol := s.db.Collection(models.ColRawData)

	// Get desktop vs mobile CTR per page
	pipeline := []bson.D{
		{{Key: "$match", Value: bson.D{
			{Key: "device", Value: bson.D{{Key: "$in", Value: bson.A{"DESKTOP", "MOBILE"}}}},
		}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: bson.D{
				{Key: "page", Value: "$page"},
				{Key: "device", Value: "$device"},
			}},
			{Key: "avgCTR", Value: bson.D{{Key: "$avg", Value: "$ctr"}}},
			{Key: "totalImpressions", Value: bson.D{{Key: "$sum", Value: "$impressions"}}},
			{Key: "topQuery", Value: bson.D{{Key: "$first", Value: "$query"}}},
		}}},
		{{Key: "$match", Value: bson.D{
			{Key: "totalImpressions", Value: bson.D{{Key: "$gte", Value: 100}}},
		}}},
	}

	cursor, err := rawCol.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var rows []struct {
		ID struct {
			Page   string `bson:"page"`
			Device string `bson:"device"`
		} `bson:"_id"`
		AvgCTR           float64 `bson:"avgCTR"`
		TotalImpressions int64   `bson:"totalImpressions"`
		TopQuery         string  `bson:"topQuery"`
	}
	cursor.All(ctx, &rows)

	// Build desktop vs mobile map per page
	type deviceStats struct {
		ctr  float64
		impr int64
	}
	pageDevices := map[string]map[string]deviceStats{}
	topQueries := map[string]string{}
	for _, r := range rows {
		if pageDevices[r.ID.Page] == nil {
			pageDevices[r.ID.Page] = map[string]deviceStats{}
		}
		pageDevices[r.ID.Page][r.ID.Device] = deviceStats{ctr: r.AvgCTR, impr: r.TotalImpressions}
		topQueries[r.ID.Page] = r.TopQuery
	}

	var issues []models.SeoIssue
	for page, devices := range pageDevices {
		desktop := devices["DESKTOP"]
		mobile := devices["MOBILE"]

		// Mobile CTR is significantly lower than desktop (>50% lower)
		if desktop.ctr > 2 && mobile.ctr > 0 && mobile.impr > 50 {
			gap := (desktop.ctr - mobile.ctr) / desktop.ctr * 100
			if gap > 50 {
				mobileShare := float64(mobile.impr) / float64(desktop.impr+mobile.impr) * 100
				issues = append(issues, models.SeoIssue{
					Page:          page,
					TopQuery:      topQueries[page],
					IssueType:     models.IssueTypeMobileGap,
					PriorityScore: float64(mobile.impr) * 0.5,
					Metrics: models.PageMetrics{
						AvgCTR:           mobile.ctr,
						AvgPosition:      0,
						TotalImpressions: mobile.impr,
						TotalClicks:      0,
					},
					Device:      "MOBILE",
					MobileShare: mobileShare,
					Status:      models.IssuePendingGeneration,
					DetectedAt:  time.Now(),
				})
			}
		}
	}

	log.Printf("[analyst] Found %d mobile gap issues", len(issues))
	return issues, nil
}

// detectCannibalization finds queries that rank on multiple pages (keyword cannibalization).
func (s *Server) detectCannibalization(ctx context.Context) ([]models.SeoIssue, error) {
	rawCol := s.db.Collection(models.ColRawData)

	// Find queries that appear on 2+ pages
	pipeline := []bson.D{
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$query"},
			{Key: "pages", Value: bson.D{{Key: "$addToSet", Value: "$page"}}},
			{Key: "totalImpressions", Value: bson.D{{Key: "$sum", Value: "$impressions"}}},
			{Key: "avgCTR", Value: bson.D{{Key: "$avg", Value: "$ctr"}}},
			{Key: "avgPosition", Value: bson.D{{Key: "$avg", Value: "$position"}}},
		}}},
		{{Key: "$addFields", Value: bson.D{
			{Key: "pageCount", Value: bson.D{{Key: "$size", Value: "$pages"}}},
		}}},
		{{Key: "$match", Value: bson.D{
			{Key: "pageCount", Value: bson.D{{Key: "$gte", Value: 2}}},
			{Key: "totalImpressions", Value: bson.D{{Key: "$gte", Value: 200}}},
		}}},
		{{Key: "$sort", Value: bson.D{{Key: "totalImpressions", Value: -1}}}},
		{{Key: "$limit", Value: int64(20)}},
	}

	cursor, err := rawCol.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var rows []struct {
		Query            string   `bson:"_id"`
		Pages            []string `bson:"pages"`
		TotalImpressions int64    `bson:"totalImpressions"`
		AvgCTR           float64  `bson:"avgCTR"`
		AvgPosition      float64  `bson:"avgPosition"`
	}
	cursor.All(ctx, &rows)

	var issues []models.SeoIssue
	for _, r := range rows {
		// Create issue for each cannibalizing page (except the best one)
		for _, page := range r.Pages[1:] { // skip first (assumed best)
			issues = append(issues, models.SeoIssue{
				Page:          page,
				TopQuery:      r.Query,
				IssueType:     models.IssueTypeCannibalization,
				PriorityScore: float64(r.TotalImpressions) * 0.3,
				Metrics: models.PageMetrics{
					AvgCTR:           r.AvgCTR,
					AvgPosition:      r.AvgPosition,
					TotalImpressions: r.TotalImpressions,
				},
				Cluster:    services.ClusterKey(r.Query),
				Status:     models.IssuePendingGeneration,
				DetectedAt: time.Now(),
			})
		}
	}

	log.Printf("[analyst] Found %d cannibalization issues", len(issues))
	return issues, nil
}
