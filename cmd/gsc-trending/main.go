package main

import (
	"context"
	"fmt"
	"log"

	"github.com/91astro/seo-agent/internal/services"
)

func main() {
	gsc, err := services.NewGSCService("./gsc-credentials.json", "sc-domain:91astrology.com")
	if err != nil { log.Fatalf("init GSC: %v", err) }
	rows, err := gsc.FetchTrendingQueries(context.Background(), 7, 20)
	if err != nil { log.Fatalf("fetch: %v", err) }
	fmt.Printf("%-4s %-55s %10s %10s %8s %10s\n", "#", "QUERY", "RECENT", "PRIOR", "POS", "GROWTH")
	for i, r := range rows {
		if i >= 40 { break }
		q := r.Query
		if len(q) > 55 { q = q[:52]+"..." }
		fmt.Printf("%-4d %-55s %10d %10d %8.1f %+9.0f%%\n", i+1, q, r.RecentImpressions, r.PriorImpressions, r.AvgPosition, r.GrowthRatio*100)
	}
}
