package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/91astro/seo-agent/config"
	"github.com/91astro/seo-agent/internal/api"
	"github.com/91astro/seo-agent/internal/db"
	"github.com/91astro/seo-agent/internal/scheduler"
	"github.com/91astro/seo-agent/internal/workers"
	"github.com/joho/godotenv"
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, reading from environment")
	}

	cfg := config.Load()

	// Connect MongoDB
	mongoClient, err := db.Connect(cfg.MongoURI)
	if err != nil {
		log.Fatalf("MongoDB connection failed: %v", err)
	}
	defer mongoClient.Disconnect(context.Background())

	database := mongoClient.Database(cfg.MongoDB)

	// Start Asynq worker server (processes jobs)
	workerServer := workers.NewServer(cfg.RedisAddr, database, cfg)
	go func() {
		if err := workerServer.Start(); err != nil {
			log.Fatalf("Worker server failed: %v", err)
		}
	}()

	// Start CMS topic poller — picks up regeneration requests set by the dashboard
	workerServer.StartCMSTopicPoller(cfg.RedisAddr, cfg.RedisPassword)

	// Start cron scheduler (enqueues jobs on schedule)
	sched := scheduler.New(cfg.RedisAddr, cfg)
	go sched.Start()

	// Start HTTP API server
	apiServer := api.NewServer(cfg, database)
	go func() {
		if err := apiServer.Run(":" + cfg.Port); err != nil {
			log.Fatalf("API server failed: %v", err)
		}
	}()

	log.Printf("91Astro SEO Agent running on :%s", cfg.Port)

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down...")
	workerServer.Shutdown()
	sched.Stop()
}
