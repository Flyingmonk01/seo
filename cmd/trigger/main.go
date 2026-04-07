// cmd/trigger/main.go
// Manually enqueue any job for testing without waiting for cron.
//
// Usage:
//   go run ./cmd/trigger <task>
//
// Examples:
//   go run ./cmd/trigger ingest
//   go run ./cmd/trigger analyze
//   go run ./cmd/trigger content
//   go run ./cmd/trigger research
//   go run ./cmd/trigger report
//   go run ./cmd/trigger blog           (daily blog creation — 5 posts)

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/hibiken/asynq"
	"github.com/joho/godotenv"
	"github.com/91astro/seo-agent/internal/workers"
)

func main() {
	godotenv.Load()

	if len(os.Args) < 2 {
		fmt.Println("Usage: go run ./cmd/trigger <task>")
		fmt.Println("Tasks: ingest, analyze, content, research, plan, report, blog")
		os.Exit(1)
	}

	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	client := asynq.NewClient(asynq.RedisClientOpt{
		Addr:     redisAddr,
		Password: os.Getenv("REDIS_PASSWORD"),
	})
	defer client.Close()

	taskName := os.Args[1]

	taskType, payload, queue := resolveTask(taskName)
	if taskType == "" {
		log.Fatalf("Unknown task: %s", taskName)
	}

	data, _ := json.Marshal(payload)
	task := asynq.NewTask(taskType, data)

	info, err := client.Enqueue(task, asynq.Queue(queue), asynq.MaxRetry(1))
	if err != nil {
		log.Fatalf("Failed to enqueue: %v", err)
	}

	fmt.Printf("✓ Enqueued task\n")
	fmt.Printf("  Type:  %s\n", info.Type)
	fmt.Printf("  ID:    %s\n", info.ID)
	fmt.Printf("  Queue: %s\n", info.Queue)
	fmt.Printf("\nWatch progress at http://localhost:4000/dashboard\n")
}

func resolveTask(name string) (taskType string, payload map[string]interface{}, queue string) {
	switch name {
	case "ingest":
		return workers.TaskIngest, nil, "critical"
	case "analyze":
		return workers.TaskAnalyze, map[string]interface{}{"date": "today"}, "default"
	case "content":
		return workers.TaskGenerateContent, map[string]interface{}{"top_n": 5}, "default"
	case "track":
		return workers.TaskTrackImpact, nil, "default"
	case "report":
		return workers.TaskReport, nil, "low"
	case "research":
		return workers.TaskResearch, map[string]interface{}{"top_n": 3}, "default"
	case "plan":
		return workers.TaskPlanFeature, nil, "default"
	case "code":
		return workers.TaskCodeFeature, nil, "low"
	case "blog":
		count := 5
		if len(os.Args) > 2 {
			if n, err := fmt.Sscanf(os.Args[2], "%d", &count); n == 1 && err == nil {
				// use provided count
			}
		}
		return workers.TaskDailyBlogCreate, map[string]interface{}{"max_posts": count}, "default"
	default:
		return "", nil, ""
	}
}
