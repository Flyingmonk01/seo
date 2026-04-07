package scheduler

import (
	"encoding/json"
	"log"

	"github.com/91astro/seo-agent/config"
	"github.com/91astro/seo-agent/internal/workers"
	"github.com/hibiken/asynq"
)

type Scheduler struct {
	asynqSched *asynq.Scheduler
	cfg        *config.Config
}

func New(redisAddr string, cfg *config.Config) *Scheduler {
	sched := asynq.NewScheduler(
		asynq.RedisClientOpt{Addr: redisAddr, Password: cfg.RedisPassword},
		&asynq.SchedulerOpts{
			LogLevel: asynq.WarnLevel,
		},
	)
	return &Scheduler{asynqSched: sched, cfg: cfg}
}

func (s *Scheduler) Start() {
	// ── Pipeline 1: SEO Metadata ──────────────────────────────────────────────

	// Daily 06:00 — ingest GSC data (chains to analyze automatically)
	s.register("0 6 * * *", workers.TaskIngest, nil, "critical")

	// Weekly Sunday 09:00 — generate content for top 20 issues
	s.register("0 9 * * 0", workers.TaskGenerateContent, map[string]int{"top_n": 20}, "default")

	// Weekly Sunday 09:00 — track impact of live changes (runs in parallel)
	s.register("0 9 * * 0", workers.TaskTrackImpact, nil, "default")

	// Weekly Sunday 10:00 — send report (after track runs)
	s.register("0 10 * * 0", workers.TaskReport, nil, "low")

	// ── Pipeline 2: Feature Development ──────────────────────────────────────

	// Monday 08:00 — research top 5 pages
	s.register("0 8 * * 1", workers.TaskResearch, map[string]int{"top_n": 5}, "default")

	// ── Pipeline 3: Page & Widget Optimization (no blog modifications) ───────

	// Thursday 08:00 — add FAQ blocks to high-query PAGES
	s.register("0 8 * * 4", workers.TaskGenerateFAQ, map[string]int{"max_pages": 10}, "default")

	// ── Pipeline 4: Daily Blog Content Generation ────────────────────────────

	// Daily 07:00 — create 10 new blog posts targeting content gaps
	// s.register("0 7 * * *", workers.TaskDailyBlogCreate, map[string]int{"max_posts": 10}, "default")

	// Daily 09:00 IST (03:30 UTC) — create blog posts for festivals, celebrity, trending astro topics
	s.register("30 3 * * *", workers.TaskCalendarBlogCreate, map[string]any{"max_posts": 5, "look_ahead_days": 45}, "default")

	if err := s.asynqSched.Run(); err != nil {
		log.Fatalf("Scheduler failed: %v", err)
	}
}

func (s *Scheduler) Stop() {
	s.asynqSched.Shutdown()
}

func (s *Scheduler) register(cronExpr, taskType string, payload interface{}, queue string) {
	task, err := newTask(taskType, payload)
	if err != nil {
		log.Printf("WARN: build task %s: %v", taskType, err)
		return
	}
	entryID, err := s.asynqSched.Register(cronExpr, task, asynq.Queue(queue))
	if err != nil {
		log.Printf("WARN: register %s: %v", taskType, err)
		return
	}
	log.Printf("Scheduled %s [%s] id=%s", taskType, cronExpr, entryID)
}

func newTask(taskType string, payload interface{}) (*asynq.Task, error) {
	if payload == nil {
		return asynq.NewTask(taskType, nil), nil
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return asynq.NewTask(taskType, data), nil
}
