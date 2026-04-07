// Seed MongoDB with realistic mock GSC data for testing the pipeline
// without real GSC credentials.
//
// Usage: go run ./cmd/seed

package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"time"

	"github.com/joho/godotenv"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"github.com/91astro/seo-agent/internal/db"
	"github.com/91astro/seo-agent/internal/models"
	"github.com/91astro/seo-agent/config"
)

// Real 91Astrology pages and queries for realistic testing
var pages = []struct {
	page    string
	queries []struct{ query string; pos float64; impr int64; ctr float64 }
}{
	{
		page: "https://91astrology.com/kundli-matching",
		queries: []struct{ query string; pos float64; impr int64; ctr float64 }{
			{"kundli matching online", 8.3, 12400, 1.2},
			{"free kundli milan", 11.1, 8200, 0.8},
			{"gun milan calculator", 9.4, 6700, 1.5},
			{"kundli matching by name", 13.2, 4300, 0.6},
			{"how to match kundli", 7.8, 3900, 2.1},
		},
	},
	{
		page: "https://91astrology.com/numerology-calculator",
		queries: []struct{ query string; pos float64; impr int64; ctr float64 }{
			{"numerology calculator", 6.2, 18700, 3.1},
			{"life path number calculator", 8.9, 9400, 2.4},
			{"numerology number finder", 12.1, 5600, 1.1},
			{"what is my numerology number", 9.7, 4100, 1.8},
			{"how to calculate numerology", 11.3, 3200, 0.9},
		},
	},
	{
		page: "https://91astrology.com/horoscope-today",
		queries: []struct{ query string; pos float64; impr int64; ctr float64 }{
			{"today horoscope", 4.1, 42000, 8.2},
			{"daily horoscope", 5.3, 31000, 6.7},
			{"horoscope today in hindi", 7.2, 19000, 4.1},
			{"aries horoscope today", 6.8, 14000, 5.3},
			{"taurus horoscope today", 7.1, 12000, 4.9},
		},
	},
	{
		page: "https://91astrology.com/free-kundli",
		queries: []struct{ query string; pos float64; impr int64; ctr float64 }{
			{"free kundli online", 9.2, 28000, 1.4},
			{"janam kundli", 10.4, 21000, 1.1},
			{"birth chart free", 12.7, 14000, 0.7},
			{"online kundli generation", 8.6, 9800, 1.9},
			{"free birth chart in hindi", 13.9, 7200, 0.5},
		},
	},
	{
		page: "https://91astrology.com/palm-reading",
		queries: []struct{ query string; pos float64; impr int64; ctr float64 }{
			{"palm reading online", 14.2, 8900, 0.4},
			{"palmistry lines meaning", 11.8, 6700, 0.9},
			{"hand reading astrology", 13.1, 4200, 0.6},
			{"how to read palm", 9.3, 3800, 1.7},
			{"palmistry online free", 15.6, 2900, 0.3},
		},
	},
}

func main() {
	godotenv.Load()
	cfg := config.Load()

	client, err := db.Connect(cfg.MongoURI)
	if err != nil {
		log.Fatalf("MongoDB: %v", err)
	}
	defer client.Disconnect(context.Background())

	database := client.Database(cfg.MongoDB)
	rawCol := database.Collection(models.ColRawData)

	// Drop existing seed data
	rawCol.DeleteMany(context.Background(), bson.D{{Key: "seeded", Value: true}})

	var docs []interface{}
	now := time.Now()

	// Generate 30 days of data
	for daysBack := 30; daysBack >= 1; daysBack-- {
		date := now.AddDate(0, 0, -daysBack).Format("2006-01-02")

		for _, p := range pages {
			for _, q := range p.queries {
				// Add some realistic variance day-to-day
				variance := (rand.Float64() - 0.5) * 0.3
				docs = append(docs, bson.M{
					"page":        p.page,
					"query":       q.query,
					"clicks":      int64(float64(q.impr) * (q.ctr/100) * (1 + variance)),
					"impressions": q.impr + int64(rand.Int63n(500)-250),
					"ctr":         q.ctr * (1 + variance),
					"position":    q.pos + (rand.Float64()-0.5)*2,
					"date":        date,
					"locale":      "en",
					"created_at":  now,
					"seeded":      true, // mark as seed data
				})
			}
		}
	}

	opts := options.InsertMany().SetOrdered(false)
	result, err := rawCol.InsertMany(context.Background(), docs, opts)
	if err != nil && !mongo.IsDuplicateKeyError(err) {
		log.Fatalf("Insert: %v", err)
	}

	fmt.Printf("✓ Seeded %d GSC rows across %d pages (30 days)\n", len(result.InsertedIDs), len(pages))
	fmt.Printf("✓ Database: %s\n", cfg.MongoDB)
	fmt.Println()
	fmt.Println("Now run the pipeline:")
	fmt.Println("  Terminal 1: make dev")
	fmt.Println("  Terminal 2: make trigger task=analyze")
	fmt.Println("  Terminal 3: make trigger task=content")
	fmt.Println("  Dashboard:  http://localhost:4000/dashboard")
}
