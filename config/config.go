package config

import "os"

type Config struct {
	// Server
	Port string
	Env  string

	// MongoDB
	MongoURI string
	MongoDB  string

	// Redis (Asynq)
	RedisAddr     string
	RedisPassword string

	// Google Search Console
	GSCCredentialsPath string
	GSCSiteURL         string

	// OpenAI
	OpenAIAPIKey string
	OpenAIModel  string

	// Bitbucket
	BitbucketToken     string
	BitbucketWorkspace string
	WebsiteRepo        string
	APIRepo            string

	// Main stack (91astro-api + website)
	MainAPIURL        string
	InternalAPIKey    string
	NextRevalidateURL string
	RevalidateSecret  string

	// Payload CMS (cms1.91astrology.com)
	CMSURL      string
	CMSEmail    string
	CMSPassword string

	// JWT (shared with 91astro-api)
	JWTSecret      string
	DashboardToken string // static token for local dev — set any string in .env

	// Email (Nodemailer equivalent)
	SMTPHost     string
	SMTPPort     string
	SMTPUser     string
	SMTPPassword string
	ReportEmail  string

	// Google Analytics
	GAPropertyID      string
	GACredentialsPath string

	// Datadog
	DatadogAPIKey string
	DatadogAppKey string
}

func Load() *Config {
	return &Config{
		Port: getEnv("PORT", "4000"),
		Env:  getEnv("ENV", "development"),

		MongoURI: getEnv("MONGODB_URI", "mongodb://localhost:27017"),
		MongoDB:  getEnv("MONGODB_NAME", "91astro_seo"),

		RedisAddr:     getEnv("REDIS_ADDR", "localhost:6379"),
		RedisPassword: getEnv("REDIS_PASSWORD", ""),

		GSCCredentialsPath: getEnv("GSC_CREDENTIALS_PATH", "./gsc-credentials.json"),
		GSCSiteURL:         getEnv("GSC_SITE_URL", "sc-domain:91astrology.com"),

		OpenAIAPIKey: getEnv("OPENAI_API_KEY", ""),
		OpenAIModel:  getEnv("OPENAI_MODEL", "gpt-4o"),

		BitbucketToken:     getEnv("BITBUCKET_TOKEN", ""),
		BitbucketWorkspace: getEnv("BITBUCKET_WORKSPACE", ""),
		WebsiteRepo:        getEnv("WEBSITE_REPO", "91astro-website"),
		APIRepo:            getEnv("API_REPO", "91astro-api"),

		MainAPIURL:        getEnv("MAIN_API_URL", "https://api.91astrology.com"),
		InternalAPIKey:    getEnv("INTERNAL_API_KEY", ""),
		NextRevalidateURL: getEnv("NEXT_REVALIDATE_URL", "https://91astrology.com"),
		RevalidateSecret:  getEnv("REVALIDATE_SECRET", ""),

		CMSURL:      getEnv("CMS_URL", "https://cms1.91astrology.com"),
		CMSEmail:    getEnv("CMS_EMAIL", ""),
		CMSPassword: getEnv("CMS_PASSWORD", ""),

		JWTSecret:      getEnv("JWT_SECRET", ""),
		DashboardToken: getEnv("DASHBOARD_TOKEN", ""),

		SMTPHost:     getEnv("SMTP_HOST", ""),
		SMTPPort:     getEnv("SMTP_PORT", "587"),
		SMTPUser:     getEnv("SMTP_USER", ""),
		SMTPPassword: getEnv("SMTP_PASSWORD", ""),
		ReportEmail:  getEnv("REPORT_EMAIL", ""),

		GAPropertyID:      getEnv("GA_PROPERTY_ID", ""),
		GACredentialsPath: getEnv("GA_CREDENTIALS_PATH", "./ga-credentials.json"),

		DatadogAPIKey: getEnv("DATADOG_API_KEY", ""),
		DatadogAppKey: getEnv("DATADOG_APP_KEY", ""),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
