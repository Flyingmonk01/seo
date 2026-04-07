package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/mongo"
	"github.com/91astro/seo-agent/config"
	"github.com/91astro/seo-agent/internal/api/handlers"
	"github.com/91astro/seo-agent/internal/api/middleware"
	"github.com/91astro/seo-agent/internal/dashboard"
	"github.com/91astro/seo-agent/internal/services"
)

func NewServer(cfg *config.Config, db *mongo.Database) *gin.Engine {
	if cfg.Env == "production" {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())

	// CORS — allow admin dashboard origins
	allowedOrigins := map[string]bool{
		"https://91astrology.com":     true,
		"https://www.91astrology.com": true,
		"http://localhost:3000":        true,
	}
	r.Use(func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if allowedOrigins[origin] {
			c.Header("Access-Control-Allow-Origin", origin)
		}
		c.Header("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Authorization,Content-Type")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	})

	cms := services.NewCMSService(cfg.CMSURL, cfg.CMSEmail, cfg.CMSPassword)
	execute := services.NewExecuteService(cfg.NextRevalidateURL, cfg.RevalidateSecret, cms)

	queueHandler := handlers.NewQueueHandler(db, execute)
	statsHandler := handlers.NewStatsHandler(db)
	openclawHandler := handlers.NewOpenClawHandler(db)

	// Dashboard UI — served at /dashboard (no auth, token entered in browser)
	dashboard.Register(r)

	// Public
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// Protected — accepts static DASHBOARD_TOKEN (dev) or real JWT (prod)
	auth := r.Group("/", middleware.Auth(cfg.JWTSecret, cfg.DashboardToken))
	{
		// Pipeline 1: metadata approval
		auth.GET("/queue", queueHandler.ListQueue)
		auth.PUT("/queue/approve/:id", queueHandler.Approve)
		auth.PUT("/queue/revert/:id", queueHandler.Revert)
		auth.PUT("/queue/reject/:id", queueHandler.Reject)

		// Pipeline 2: feature development
		auth.GET("/features", queueHandler.ListFeatures)
		auth.PUT("/features/:id/flag", queueHandler.SetFeatureFlag)

		// Stats + reports
		auth.GET("/stats", statsHandler.GetStats)
		auth.GET("/results", statsHandler.GetResults)
		auth.GET("/learnings", statsHandler.GetLearnings)

		// OpenClaw webhook — polls for pending code tasks + reports results
		auth.GET("/openclaw/tasks", openclawHandler.PollTask)
		auth.POST("/openclaw/tasks/:id/result", openclawHandler.ReportResult)
		auth.GET("/openclaw/tasks/pending/count", openclawHandler.PendingCount)
		auth.GET("/openclaw/tasks/list", openclawHandler.ListTasks)

	}

	return r
}
