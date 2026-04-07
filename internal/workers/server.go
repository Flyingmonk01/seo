package workers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/mongo"
	"github.com/91astro/seo-agent/config"
	"github.com/91astro/seo-agent/internal/agent"
	"github.com/91astro/seo-agent/internal/services"
)

type Server struct {
	asynqServer *asynq.Server
	db          *mongo.Database
	cfg         *config.Config
	gsc         *services.GSCService
	openai      *services.OpenAIService
	bitbucket   *services.BitbucketService
	execute     *services.ExecuteService
	cms         *services.CMSService
	analytics   *services.AnalyticsService
	mail        *services.MailService
	prokerala   *services.ProkeralaService

	// Agent layer
	seoAgent *agent.SEOAgent
}

func NewServer(redisAddr string, db *mongo.Database, cfg *config.Config) *Server {
	gsc, err := services.NewGSCService(cfg.GSCCredentialsPath, cfg.GSCSiteURL)
	if err != nil {
		log.Fatalf("GSC init: %v", err)
	}

	return &Server{
		asynqServer: asynq.NewServer(
			asynq.RedisClientOpt{Addr: redisAddr, Password: cfg.RedisPassword},
			asynq.Config{
				Concurrency: 5,
				Queues:      map[string]int{"critical": 6, "default": 3, "low": 1},
				ErrorHandler: asynq.ErrorHandlerFunc(func(ctx context.Context, task *asynq.Task, err error) {
					log.Printf("ERROR task=%s: %v", task.Type(), err)
				}),
			},
		),
		db:        db,
		cfg:       cfg,
		gsc:       gsc,
		openai:    services.NewOpenAIService(cfg.OpenAIAPIKey, cfg.OpenAIModel),
		bitbucket: services.NewBitbucketService(cfg.BitbucketToken, cfg.BitbucketWorkspace),
		cms:       services.NewCMSService(cfg.CMSURL, cfg.CMSEmail, cfg.CMSPassword),
		execute:   services.NewExecuteService(cfg.NextRevalidateURL, cfg.RevalidateSecret, services.NewCMSService(cfg.CMSURL, cfg.CMSEmail, cfg.CMSPassword)),
		analytics: services.NewAnalyticsService(cfg.GAPropertyID, cfg.GACredentialsPath, cfg.DatadogAPIKey, cfg.DatadogAppKey),
		mail:      services.NewMailService(cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPUser, cfg.SMTPPassword),
		prokerala: services.NewProkeralaService(cfg.ProkeralaClientID, cfg.ProkeralaClientSecret),
		seoAgent:  agent.NewSEOAgent(services.NewOpenAIService(cfg.OpenAIAPIKey, cfg.OpenAIModel)),
	}
}

func (s *Server) Start() error {
	mux := asynq.NewServeMux()

	// Pipeline 1
	mux.HandleFunc(TaskIngest, s.handleIngest)
	mux.HandleFunc(TaskAnalyze, s.handleAnalyze)
	mux.HandleFunc(TaskGenerateContent, s.handleGenerateContent)
	mux.HandleFunc(TaskTrackImpact, s.handleTrackImpact)
	mux.HandleFunc(TaskReport, s.handleReport)

	// Pipeline 2
	mux.HandleFunc(TaskResearch, s.handleResearch)
	mux.HandleFunc(TaskPlanFeature, s.handlePlanFeature)
	mux.HandleFunc(TaskCodeFeature, s.handleCodeFeature)
	mux.HandleFunc(TaskMonitorFeature, s.handleMonitorFeature)

	// Pipeline 3: Page & Widget Optimization
	mux.HandleFunc(TaskCreateContent, s.handleCreateContent)
	mux.HandleFunc(TaskGenerateFAQ, s.handleGenerateFAQ)
	mux.HandleFunc(TaskRefreshContent, s.handleRefreshContent)
	mux.HandleFunc(TaskInternalLink, s.handleInternalLink)

	// Pipeline 4: Daily Blog Content Generation
	mux.HandleFunc(TaskDailyBlogCreate, s.handleDailyBlogCreate)
	mux.HandleFunc(TaskRegenerateBlog, s.handleRegenerateBlog)
	mux.HandleFunc(TaskCalendarBlogCreate, s.handleCalendarBlogCreate)

	return s.asynqServer.Run(mux)
}

func (s *Server) Shutdown() {
	s.asynqServer.Shutdown()
}

// enqueue is a helper used by workers to chain next tasks
func (s *Server) enqueue(taskType string, payload interface{}, opts ...asynq.Option) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	client := asynq.NewClient(asynq.RedisClientOpt{
		Addr:     s.cfg.RedisAddr,
		Password: s.cfg.RedisPassword,
	})
	defer client.Close()
	_, err = client.Enqueue(asynq.NewTask(taskType, data), opts...)
	return err
}
