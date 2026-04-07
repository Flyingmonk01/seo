package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/oauth2/google"
)

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

type AnalyticsService struct {
	gaPropertyID      string
	gaCredentialsPath string
	datadogAPIKey     string
	datadogAppKey     string
	httpClient        *http.Client
}

type PageBehavior struct {
	BounceRate         float64
	AvgSessionDuration float64
	ConversionRate     float64
	ErrorRate          float64
	PageViews          int64
}

type RUMData struct {
	RageClicks  []RageClick
	DeadClicks  []DeadClick
	ErrorEvents []ErrorEvent
}

type RageClick struct {
	Selector   string
	Count      int
	PagePath   string
}

type DeadClick struct {
	Selector string
	Count    int
}

type ErrorEvent struct {
	Message string
	Count   int
}

func NewAnalyticsService(gaPropertyID, gaCredentialsPath, datadogAPIKey, datadogAppKey string) *AnalyticsService {
	return &AnalyticsService{
		gaPropertyID:      gaPropertyID,
		gaCredentialsPath: gaCredentialsPath,
		datadogAPIKey:     datadogAPIKey,
		datadogAppKey:     datadogAppKey,
		httpClient:        &http.Client{Timeout: 30 * time.Second},
	}
}

// GetPageBehavior fetches GA4 metrics for a specific page over the last N days.
func (a *AnalyticsService) GetPageBehavior(ctx context.Context, pagePath string, days int) (*PageBehavior, error) {
	// GA4 Data API call
	// Using REST API directly for simplicity — can swap to client library
	startDate := time.Now().AddDate(0, 0, -days).Format("2006-01-02")
	endDate := time.Now().Format("2006-01-02")

	reqBody := map[string]interface{}{
		"dateRanges": []map[string]string{
			{"startDate": startDate, "endDate": endDate},
		},
		"dimensions": []map[string]string{
			{"name": "pagePath"},
		},
		"metrics": []map[string]string{
			{"name": "bounceRate"},
			{"name": "averageSessionDuration"},
			{"name": "conversions"},
			{"name": "screenPageViews"},
		},
		"dimensionFilter": map[string]interface{}{
			"filter": map[string]interface{}{
				"fieldName":    "pagePath",
				"stringFilter": map[string]string{"value": pagePath, "matchType": "EXACT"},
			},
		},
	}

	body, _ := json.Marshal(reqBody)
	url := fmt.Sprintf("https://analyticsdata.googleapis.com/v1beta/properties/%s:runReport", a.gaPropertyID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytesReader(body))
	if err != nil {
		return nil, err
	}

	token, err := a.getGAToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("GA token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GA API: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Rows []struct {
			MetricValues []struct{ Value string } `json:"metricValues"`
		} `json:"rows"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode GA response: %w", err)
	}

	behavior := &PageBehavior{}
	if len(result.Rows) > 0 {
		row := result.Rows[0]
		if len(row.MetricValues) >= 4 {
			fmt.Sscanf(row.MetricValues[0].Value, "%f", &behavior.BounceRate)
			fmt.Sscanf(row.MetricValues[1].Value, "%f", &behavior.AvgSessionDuration)
			fmt.Sscanf(row.MetricValues[2].Value, "%f", &behavior.ConversionRate)
			fmt.Sscanf(row.MetricValues[3].Value, "%d", &behavior.PageViews)
		}
	}

	return behavior, nil
}

// GetRUMData fetches Datadog RUM events for rage clicks and errors.
func (a *AnalyticsService) GetRUMData(ctx context.Context, pagePath string) (*RUMData, error) {
	now := time.Now().Unix()
	from := time.Now().AddDate(0, 0, -7).Unix()

	// Datadog RUM API
	rageClicksURL := fmt.Sprintf(
		"https://api.datadoghq.com/api/v2/rum/events?filter[query]=@type:action @action.type:click @action.frustration.type:rage_click @view.url_path:%s&filter[from]=%d&filter[to]=%d",
		pagePath, from, now,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rageClicksURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("DD-API-KEY", a.datadogAPIKey)
	req.Header.Set("DD-APPLICATION-KEY", a.datadogAppKey)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Datadog API: %w", err)
	}
	defer resp.Body.Close()

	var ddResp struct {
		Data []struct {
			Attributes struct {
				Action struct {
					Target struct{ Name string } `json:"target"`
				} `json:"action"`
			} `json:"attributes"`
		} `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&ddResp)

	rumData := &RUMData{}
	seen := map[string]int{}
	for _, event := range ddResp.Data {
		selector := event.Attributes.Action.Target.Name
		seen[selector]++
	}
	for selector, count := range seen {
		rumData.RageClicks = append(rumData.RageClicks, RageClick{
			Selector: selector,
			Count:    count,
			PagePath: pagePath,
		})
	}

	return rumData, nil
}

func (a *AnalyticsService) getGAToken(ctx context.Context) (string, error) {
	data, err := readFile(a.gaCredentialsPath)
	if err != nil {
		return "", err
	}
	creds, err := google.CredentialsFromJSON(ctx, data,
		"https://www.googleapis.com/auth/analytics.readonly",
	)
	if err != nil {
		return "", err
	}
	token, err := creds.TokenSource.Token()
	if err != nil {
		return "", err
	}
	return token.AccessToken, nil
}
