package services

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type ProkeralaService struct {
	clientID     string
	clientSecret string
	httpClient   *http.Client

	mu         sync.Mutex
	token      string
	tokenExpAt time.Time
}

func NewProkeralaService(clientID, clientSecret string) *ProkeralaService {
	return &ProkeralaService{
		clientID:     clientID,
		clientSecret: clientSecret,
		httpClient:   &http.Client{Timeout: 10 * time.Second},
	}
}

func (p *ProkeralaService) IsConfigured() bool {
	return p.clientID != "" && p.clientSecret != ""
}

func (p *ProkeralaService) getToken() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.token != "" && time.Now().Before(p.tokenExpAt) {
		return p.token, nil
	}

	data := url.Values{}
	data.Set("grant_type", "client_credentials")
	data.Set("client_id", p.clientID)
	data.Set("client_secret", p.clientSecret)

	resp, err := p.httpClient.Post("https://api.prokerala.com/token",
		"application/x-www-form-urlencoded", strings.NewReader(data.Encode()))
	if err != nil {
		return "", fmt.Errorf("prokerala auth: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var result struct {
		AccessToken string  `json:"access_token"`
		ExpiresIn   float64 `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &result); err != nil || result.AccessToken == "" {
		return "", fmt.Errorf("prokerala token parse: %s", string(body))
	}

	p.token = result.AccessToken
	p.tokenExpAt = time.Now().Add(time.Duration(result.ExpiresIn-60) * time.Second)
	return p.token, nil
}

// TithiAnchor is returned by a single Prokerala API call for today.
type TithiAnchor struct {
	Date       time.Time
	TithiIndex int    // 1-30 (1=Pratipada Shukla ... 15=Purnima, 16=Pratipada Krishna ... 30=Amavasya)
	TithiName  string // e.g. "Krishna Paksha Ekadashi"
}

// tithiNameToIndex maps Prokerala tithi names → 1-30 index.
var tithiNameToIndex = map[string]int{
	"Shukla Paksha Pratipada": 1, "Shukla Paksha Dwitiya": 2, "Shukla Paksha Tritiya": 3,
	"Shukla Paksha Chaturthi": 4, "Shukla Paksha Panchami": 5, "Shukla Paksha Shashthi": 6,
	"Shukla Paksha Saptami": 7, "Shukla Paksha Ashtami": 8, "Shukla Paksha Navami": 9,
	"Shukla Paksha Dashami": 10, "Shukla Paksha Ekadashi": 11, "Shukla Paksha Dwadashi": 12,
	"Shukla Paksha Trayodashi": 13, "Shukla Paksha Chaturdashi": 14, "Shukla Paksha Purnima": 15,
	"Krishna Paksha Pratipada": 16, "Krishna Paksha Dwitiya": 17, "Krishna Paksha Tritiya": 18,
	"Krishna Paksha Chaturthi": 19, "Krishna Paksha Panchami": 20, "Krishna Paksha Shashthi": 21,
	"Krishna Paksha Saptami": 22, "Krishna Paksha Ashtami": 23, "Krishna Paksha Navami": 24,
	"Krishna Paksha Dashami": 25, "Krishna Paksha Ekadashi": 26, "Krishna Paksha Dwadashi": 27,
	"Krishna Paksha Trayodashi": 28, "Krishna Paksha Chaturdashi": 29, "Krishna Paksha Amavasya": 30,
}

// FetchTodayTithi makes a single API call to get today's tithi as an anchor.
func (p *ProkeralaService) FetchTodayTithi(today time.Time) (*TithiAnchor, error) {
	token, err := p.getToken()
	if err != nil {
		return nil, err
	}

	params := url.Values{}
	params.Set("ayanamsa", "1")
	params.Set("coordinates", "28.6139,77.2090")
	params.Set("datetime", today.Format("2006-01-02")+"T06:00:00+05:30")
	params.Set("la", "en")

	req, _ := http.NewRequest("GET",
		"https://api.prokerala.com/v2/astrology/panchang?"+params.Encode(), nil)
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prokerala panchang: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var raw struct {
		Status string `json:"status"`
		Data   struct {
			Tithi []struct {
				Name   string `json:"name"`
				Paksha string `json:"paksha"`
			} `json:"tithi"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("prokerala parse: %w", err)
	}
	if raw.Status != "ok" {
		return nil, fmt.Errorf("prokerala status: %s", string(body))
	}
	if len(raw.Data.Tithi) == 0 {
		return nil, fmt.Errorf("no tithi in response")
	}

	tithiName := raw.Data.Tithi[0].Paksha + " " + raw.Data.Tithi[0].Name
	idx, ok := tithiNameToIndex[tithiName]
	if !ok {
		return nil, fmt.Errorf("unknown tithi: %s", tithiName)
	}
	return &TithiAnchor{Date: today, TithiIndex: idx, TithiName: tithiName}, nil
}

// significantTithiIndices are the tithi numbers (1-30) worth blogging about.
// Lunar month = 29.53 days, so each tithi ≈ 0.984 days.
var significantTithiIndices = map[int]struct {
	Name     string
	Category string
	Query    string
}{
	11: {"Shukla Ekadashi Vrat", "Vedic", "ekadashi vrat significance fasting benefits"},
	13: {"Pradosh Vrat", "Vedic", "pradosh vrat shiva puja significance"},
	15: {"Purnima", "Vedic", "purnima significance vedic astrology rituals"},
	26: {"Krishna Ekadashi Vrat", "Vedic", "krishna ekadashi vrat fasting significance"},
	28: {"Pradosh Vrat", "Vedic", "pradosh vrat shiva puja significance"},
	29: {"Masik Shivratri", "Vedic", "masik shivratri puja significance monthly"},
	30: {"Amavasya", "Vedic", "amavasya significance rituals ancestors"},
}

const lunarMonthDays = 29.53058770576

// ComputeSignificantDates uses today's tithi as an anchor and calculates
// all significant tithi dates within the look-ahead window — no extra API calls.
func ComputeSignificantDates(anchor *TithiAnchor, from, to time.Time) []struct {
	Date     time.Time
	Name     string
	Category string
	Query    string
} {
	// Each tithi is 1/30th of a lunar month
	tithiDuration := lunarMonthDays / 30.0 // days per tithi

	// Calculate the Gregorian date of tithi #1 (Shukla Pratipada) of the current lunar month
	// by going back (anchor.TithiIndex - 1) tithis from today
	daysBack := float64(anchor.TithiIndex-1) * tithiDuration
	lunarMonthStart := anchor.Date.Add(-time.Duration(daysBack*24) * time.Hour)

	var results []struct {
		Date     time.Time
		Name     string
		Category string
		Query    string
	}

	seen := map[string]bool{}

	// Check current and next 2 lunar months to cover 45-day window
	for month := 0; month < 3; month++ {
		monthOffset := float64(month) * lunarMonthDays
		for tithiIdx, info := range significantTithiIndices {
			daysFromStart := float64(tithiIdx-1) * tithiDuration
			tithiDate := lunarMonthStart.Add(
				time.Duration((monthOffset+daysFromStart)*24) * time.Hour,
			)
			// Round to nearest day
			tithiDate = time.Date(tithiDate.Year(), tithiDate.Month(),
				int(math.Round(float64(tithiDate.Day())+(tithiDate.Sub(time.Date(tithiDate.Year(), tithiDate.Month(), tithiDate.Day(), 0, 0, 0, 0, tithiDate.Location())).Hours()/24))),
				0, 0, 0, 0, time.UTC)

			if !tithiDate.After(from) || tithiDate.After(to) {
				continue
			}

			// Deduplicate same event in same month
			key := fmt.Sprintf("%s-%d-%d", info.Name, tithiDate.Year(), int(tithiDate.Month()))
			if seen[key] {
				continue
			}
			seen[key] = true

			month := tithiDate.Format("January")
			year := tithiDate.Year()
			results = append(results, struct {
				Date     time.Time
				Name     string
				Category string
				Query    string
			}{
				Date:     tithiDate,
				Name:     fmt.Sprintf("%s %s %d", info.Name, month, year),
				Category: info.Category,
				Query:    fmt.Sprintf("%s %s %d", info.Query, strings.ToLower(month), year),
			})
		}
	}
	return results
}
