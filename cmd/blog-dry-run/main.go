// cmd/blog-dry-run — exercise the enrichment + blog-generation pipeline for
// one trending GSC theme, without touching Redis/Mongo/CMS. Prints the
// generated heading, meta, and section titles so we can eyeball whether the
// new "current-moment" research is actually shaping the post.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/joho/godotenv"

	"github.com/91astro/seo-agent/config"
	"github.com/91astro/seo-agent/internal/services"
	"github.com/91astro/seo-agent/internal/workers"
)

func main() {
	_ = godotenv.Load()

	override := flag.String("query", "", "override the trending query (skip GSC fetch)")
	pickIdx := flag.Int("pick", 0, "0-based index into trending list to use")
	flag.Parse()

	cfg := config.Load()

	gsc, err := services.NewGSCService(cfg.GSCCredentialsPath, cfg.GSCSiteURL)
	if err != nil {
		log.Fatalf("gsc init: %v", err)
	}

	ctx := context.Background()

	primary := *override
	siblings := []string{}

	if primary == "" {
		trending, err := gsc.FetchTrendingQueries(ctx, 7, 20)
		if err != nil {
			log.Fatalf("fetch trending: %v", err)
		}
		if len(trending) <= *pickIdx {
			log.Fatalf("trending list has %d items, asked for index %d", len(trending), *pickIdx)
		}
		// pick first non-top-3 query
		idx := *pickIdx
		for i := *pickIdx; i < len(trending); i++ {
			if trending[i].AvgPosition > 3 {
				idx = i
				break
			}
		}
		primary = trending[idx].Query
		// gather siblings sharing the cluster key
		ck := services.ClusterKey(primary)
		for j, t := range trending {
			if j == idx {
				continue
			}
			if services.ClusterKey(t.Query) == ck {
				siblings = append(siblings, t.Query)
			}
		}
		fmt.Printf("Picked trending query: %q\n", primary)
		fmt.Printf("Cluster siblings: %v\n", siblings)
	}

	srv := workers.NewBareServer(cfg)

	fmt.Println("\n── Enriching theme ──")
	enrich, err := srv.EnrichThemePublic(ctx, primary, siblings)
	if err != nil {
		log.Printf("enrichment failed (continuing without): %v", err)
	} else {
		out, _ := json.MarshalIndent(enrich, "", "  ")
		fmt.Println(string(out))
	}

	fmt.Println("\n── Generating blog (strategist + writer) ──")
	post, err := srv.GeneratePostPublic(ctx, primary, 200, enrich)
	if err != nil {
		log.Fatalf("generation failed: %v", err)
	}

	fmt.Printf("\nHEADING:        %s\n", post.Heading)
	fmt.Printf("META TITLE:     %s\n", post.MetaTitle)
	fmt.Printf("META DESC:      %s\n", post.MetaDescription)
	fmt.Printf("CATEGORY:       %s\n", post.Category)
	fmt.Printf("SECTIONS (%d):\n", len(post.Paragraphs))
	for i, p := range post.Paragraphs {
		if m, ok := p.(map[string]interface{}); ok {
			if h, ok := m["Heading"].(string); ok {
				fmt.Printf("  %2d. %s\n", i+1, h)
			}
		}
	}
	fmt.Printf("IMAGE PROMPT:   %s\n", post.ImagePrompt)

	if len(os.Args) > 0 {
		_ = os.Args
	}
}
