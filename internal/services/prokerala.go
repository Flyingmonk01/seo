package services

import (
	"encoding/json"
	"fmt"
	"io"
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

	mu          sync.Mutex
	token       string
	tokenExpAt  time.Time
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

// PanchangResult holds the astrologically significant elements for a day.
type PanchangResult struct {
	Date     time.Time
	Tithi    string
	Nakshatra string
	Yoga     string
	Vaara    string // day of week
}

// FetchPanchang returns the panchang for a given date (New Delhi coordinates, IST).
func (p *ProkeralaService) FetchPanchang(date time.Time) (*PanchangResult, error) {
	token, err := p.getToken()
	if err != nil {
		return nil, err
	}

	params := url.Values{}
	params.Set("ayanamsa", "1") // Lahiri
	params.Set("coordinates", "28.6139,77.2090") // New Delhi
	params.Set("datetime", date.Format("2006-01-02")+"T06:00:00+05:30")
	params.Set("la", "en")

	req, err := http.NewRequest("GET",
		"https://api.prokerala.com/v2/astrology/panchang?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
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
			Vaara string `json:"vaara"`
			Tithi []struct {
				Name   string `json:"name"`
				Paksha string `json:"paksha"`
			} `json:"tithi"`
			Nakshatra []struct {
				Name string `json:"name"`
			} `json:"nakshatra"`
			Yoga []struct {
				Name string `json:"name"`
			} `json:"yoga"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("prokerala parse: %w", err)
	}
	if raw.Status != "ok" {
		return nil, fmt.Errorf("prokerala status: %s", string(body))
	}

	result := &PanchangResult{Date: date, Vaara: raw.Data.Vaara}
	if len(raw.Data.Tithi) > 0 {
		result.Tithi = raw.Data.Tithi[0].Paksha + " " + raw.Data.Tithi[0].Name
	}
	if len(raw.Data.Nakshatra) > 0 {
		result.Nakshatra = raw.Data.Nakshatra[0].Name
	}
	if len(raw.Data.Yoga) > 0 {
		result.Yoga = raw.Data.Yoga[0].Name
	}
	return result, nil
}

// SignificantTithis maps tithi names to the festival/vrat they represent.
// These are calendar-independent — they recur every lunar month on the same tithi.
var SignificantTithis = map[string]struct {
	Name     string
	Category string
	Query    string
}{
	"Shukla Paksha Ekadashi":  {"Ekadashi Vrat", "Vedic", "ekadashi vrat significance fasting benefits"},
	"Krishna Paksha Ekadashi": {"Ekadashi Vrat", "Vedic", "ekadashi vrat significance fasting benefits"},
	"Shukla Paksha Purnima":   {"Purnima", "Vedic", "purnima significance vedic astrology rituals"},
	"Krishna Paksha Amavasya": {"Amavasya", "Vedic", "amavasya significance rituals ancestors"},
	"Shukla Paksha Chaturthi": {"Vinayaka Chaturthi", "Festival", "vinayaka chaturthi puja vidhi significance"},
	"Shukla Paksha Navami":    {"Navami", "Festival", "navami significance vedic puja"},
	"Shukla Paksha Trayodashi":{"Pradosh Vrat", "Vedic", "pradosh vrat shiva puja significance"},
	"Krishna Paksha Trayodashi":{"Pradosh Vrat", "Vedic", "pradosh vrat shiva puja significance"},
	"Krishna Paksha Chaturdashi":{"Masik Shivratri", "Vedic", "masik shivratri puja significance monthly"},
}
