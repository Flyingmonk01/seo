# 91Astro SEO Agent — Implementation Guide

## Core Principle

**CMS is the single source of truth.** The website just renders what CMS returns.

```
 Google Search Console
       │
       │ what people search for
       ▼
 91astro-seo (Go agent)
       │
       │ GPT-4o generates better content
       │
       │ PATCH /api/pages/{id}   ← titles, meta, FAQ blocks
       │ PATCH /api/Posts/{id}   ← headings, content, paragraphs
       │ POST  /api/Posts        ← new blog posts
       ▼
 91astro-cms (Payload CMS)
       │
       │ GET /api/pages/{id}     ← website fetches on next request
       ▼
 91astro-website (Next.js)
       │
       │ renders CMS data + structured data markup
       ▼
 Google indexes → richer snippets → more traffic
```

**No 91astro-api changes. Ever.**

---

## What the Website Fetches from CMS Today

Every page on the site calls Payload CMS with hardcoded page IDs or slug lookups:

| Page | CMS Call | Fields Used for SEO |
|------|----------|-------------------|
| Homepage | `GET /api/pages/65815feb9164719d10980814` | `meta.title`, `meta.description`, `layout[]` |
| Blog post | `GET /api/Posts/{id}?locale={locale}` | `Heading` → `<title>`, `Content[0].children[0].text` → description |
| Blog listing | `GET /api/posts?limit=100&locale={locale}` | `Heading`, `slug`, `id`, `category`, `image` |
| Zodiac pages | `GET /api/pages/{zodiacPageId}?locale={locale}` | `meta.title`, `layout[]` (blocks) |
| Horoscope pages | `GET /api/pages/{horoscopePageId}?locale={locale}` | `meta.title`, `layout[]` |
| Other pages | `GET /api/pages/{pageId}?locale={locale}` | `meta.title`, `meta.description`, `layout[]` |

**Key insight:** If the SEO agent updates `meta.title` in CMS → the website's `<title>` tag changes on next ISR cycle. Same for every field.

### CMS Field → HTML Mapping

```
CMS Field                  →  What Appears in HTML
─────────────────────────────────────────────────────
meta.title                 →  <title> tag (via generateMetadata)
meta.description           →  <meta name="description">
Heading (Posts)             →  <title> tag for blogs, <h1> on page
Content[] (Posts)           →  Main article body (rich text)
Paragraph[] (Posts)         →  H2 sections with text + images
Paragraph[].Hyperlink      →  Internal links within content
layout[].FAQ               →  FAQ accordion sections
layout[].PageHeading       →  H1 page heading
layout[].content           →  Text sections with paragraphs
image.filename             →  Featured image (from S3 bucket)
```

---

## What's Missing (Why Traffic Is Low)

### From CMS Side (agent can fix by writing to Payload API)

| Problem | Impact | Fix |
|---------|--------|-----|
| Generic titles like "91Astrology" | Google shows boring snippet | Update `meta.title` with keyword-rich title |
| Empty/weak meta descriptions | Low CTR from search results | Update `meta.description` with compelling copy |
| Blog Heading != meta.title | Keyword mismatch | Sync Heading with optimized meta.title |
| No FAQ blocks on pages | Missing FAQ rich snippets | Add FAQ blocks to page `layout[]` |
| Thin blog content | Can't rank for long-tail queries | Enrich `Content` + `Paragraph` fields |
| No internal links | Poor link equity flow | Populate `Paragraph[].Hyperlink` fields |
| Stale content | Google deprioritizes old pages | Refresh Content/Paragraph with updated text |
| No new content for trending queries | Missing traffic opportunities | Create new Posts via `POST /api/Posts` |

### From Website Side (one-time code changes)

| Problem | Impact | Fix |
|---------|--------|-----|
| Zero JSON-LD structured data | No rich snippets in Google | Add `<script type="application/ld+json">` using CMS data already fetched |
| No OpenGraph image tags | No preview on social share | Use `image.filename` from CMS response in `og:image` |
| No Twitter card tags | Poor Twitter/X sharing | Add `twitter:card` meta |
| Blog description = first paragraph text | Weak auto-generated description | Use `meta.description` from CMS (agent populates it) |
| 48hr ISR cache | CMS changes take 2 days to appear | Reduce to 1hr |
| Static sitemap XMLs in /public/ | New content not discovered quickly | Dynamic sitemap from CMS API |
| FAQ blocks render but have no schema markup | Google can't identify FAQs | Wrap FAQ rendering in FAQPage JSON-LD |

---

## Implementation Plan

### Phase 1: Make the SEO Agent Fully Utilize CMS (91astro-seo)

The agent currently only updates `meta.title` and `meta.description`. Expand it to update everything the website renders.

#### 1.1 Update Heading Along with Meta

**File:** `internal/services/cms.go`

Currently `UpdatePostMeta` updates Heading and meta.description separately. But `generateMetadata` in the blog page uses `Heading` as the `<title>` tag:

```javascript
// blogs/[slug]/page.js line 404
title: blogData?.Heading || '91Astrology',
```

So the agent must update `Heading` to match the optimized title.

**Change `UpdatePostMeta`:**

```go
func (c *CMSService) UpdatePostMeta(postID, title, description string) error {
    return c.PatchDocument("Posts", postID, map[string]interface{}{
        "Heading": title,                           // ← this becomes <title> for blog pages
        "meta": map[string]string{
            "title":       title,                   // ← this is for pages that use meta.title
            "description": description,
        },
    })
}
```

This already works like this. Verified.

#### 1.2 Add FAQ Blocks to Pages via Payload API

**File:** `internal/services/cms.go` — add new method

The CMS `Pages` and `Festivals` collections have a `layout` field which is an array of blocks. FAQ block structure:

```json
{
  "blockType": "FAQ",
  "FAQ": [
    {
      "Question": "What is Aries horoscope today?",
      "Answer": [{ "Answer": "Today's Aries horoscope..." }]
    }
  ]
}
```

**New method: `AddFAQToPage(pageID string, faqs []FAQItem) error`**

```go
// AddFAQToPage appends a FAQ block to a page's layout array.
func (c *CMSService) AddFAQToPage(pageID string, faqs []models.FAQItem) error {
    // First, fetch current layout to append (not overwrite)
    doc, err := c.GetFullDocument("pages", pageID)
    if err != nil {
        return err
    }

    layout := doc["layout"].([]interface{})

    // Build FAQ block matching CMS schema
    faqEntries := make([]map[string]interface{}, len(faqs))
    for i, faq := range faqs {
        faqEntries[i] = map[string]interface{}{
            "Question": faq.Question,
            "Answer":   []map[string]string{{"Answer": faq.Answer}},
        }
    }

    faqBlock := map[string]interface{}{
        "blockType": "FAQ",
        "FAQ":       faqEntries,
    }

    layout = append(layout, faqBlock)

    return c.PatchDocument("pages", pageID, map[string]interface{}{
        "layout": layout,
    })
}
```

**Also add `GetFullDocument` to fetch with all fields:**

```go
func (c *CMSService) GetFullDocument(collection, docID string) (map[string]interface{}, error) {
    reqURL := fmt.Sprintf("%s/api/%s/%s?depth=0", c.baseURL, collection, docID)
    resp, err := c.httpClient.Get(reqURL)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()

    var doc map[string]interface{}
    json.NewDecoder(resp.Body).Decode(&doc)
    return doc, nil
}
```

#### 1.3 Create New Blog Posts via Payload API

**File:** `internal/services/cms.go` — add new method

```go
type CreatePostRequest struct {
    Title      string            `json:"title"`
    Heading    string            `json:"Heading"`
    Date       string            `json:"Date"`
    Category   string            `json:"category"`     // ObjectID
    Author     string            `json:"author"`       // ObjectID
    Image      string            `json:"image"`        // Media ObjectID
    Content    interface{}       `json:"Content"`      // Slate rich text
    Paragraph  []interface{}     `json:"Paragraph"`
    Meta       map[string]string `json:"meta"`
    Identifier string            `json:"Identifier"`   // "en"
    IsHidden   bool              `json:"isHidden"`
}

func (c *CMSService) CreatePost(post CreatePostRequest) (string, error) {
    token, err := c.GetToken()
    if err != nil {
        return "", err
    }

    body, _ := json.Marshal(post)
    req, _ := http.NewRequest(http.MethodPost,
        c.baseURL+"/api/Posts",
        bytes.NewReader(body),
    )
    req.Header.Set("Authorization", "JWT "+token)
    req.Header.Set("Content-Type", "application/json")

    resp, err := c.httpClient.Do(req)
    if err != nil {
        return "", err
    }
    defer resp.Body.Close()

    var result struct {
        Doc struct {
            ID string `json:"id"`
        } `json:"doc"`
    }
    json.NewDecoder(resp.Body).Decode(&result)
    return result.Doc.ID, nil
}
```

#### 1.4 Update Blog Content (Enrich Thin Posts)

**File:** `internal/services/cms.go` — add new method

```go
func (c *CMSService) UpdatePostContent(postID string, content interface{}, paragraphs []interface{}) error {
    fields := map[string]interface{}{}
    if content != nil {
        fields["Content"] = content
    }
    if paragraphs != nil {
        fields["Paragraph"] = paragraphs
    }
    return c.PatchDocument("Posts", postID, fields)
}
```

#### 1.5 Add Internal Links to Blog Paragraphs

**File:** `internal/services/cms.go` — add new method

The blog detail page renders `Paragraph[].Hyperlink` as a link. The agent can populate this:

```go
func (c *CMSService) AddInternalLink(postID string, paragraphIndex int, targetURL string, targetPostID string) error {
    // Fetch current post
    doc, err := c.GetFullDocument("Posts", postID)
    if err != nil {
        return err
    }

    paragraphs := doc["Paragraph"].([]interface{})
    if paragraphIndex >= len(paragraphs) {
        return fmt.Errorf("paragraph index %d out of range", paragraphIndex)
    }

    p := paragraphs[paragraphIndex].(map[string]interface{})
    p["Hyperlink"] = targetURL
    p["BlogToLink"] = targetPostID
    paragraphs[paragraphIndex] = p

    return c.PatchDocument("Posts", postID, map[string]interface{}{
        "Paragraph": paragraphs,
    })
}
```

#### 1.6 List All Posts (for Content Gap Analysis)

```go
func (c *CMSService) ListPosts(limit int, locale string) ([]map[string]interface{}, error) {
    url := fmt.Sprintf("%s/api/posts?limit=%d&locale=%s&depth=0", c.baseURL, limit, locale)
    resp, err := c.httpClient.Get(url)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()

    var result struct {
        Docs []map[string]interface{} `json:"docs"`
    }
    json.NewDecoder(resp.Body).Decode(&result)
    return result.Docs, nil
}
```

#### 1.7 New Workers

**`internal/workers/content_creator.go`** — new Asynq task `seo:create_content`

```
Flow:
1. Fetch top search queries from seo_raw_data that have NO matching page
   (high impressions, no page URL in our domain matching the query)
2. GPT-4o generates full blog post:
   - Title/Heading (keyword-optimized, max 60 chars)
   - Content (Slate rich text JSON)
   - 3-5 Paragraphs with H2 sections
   - Internal links to existing posts (Paragraph.Hyperlink)
   - Meta title + description
   - 3-5 FAQ items
3. CMSService.CreatePost() → POST /api/Posts
4. Record in seo_suggestions for tracking
```

**`internal/workers/faq_generator.go`** — new Asynq task `seo:generate_faq`

```
Flow:
1. Find pages with question-intent queries from GSC
   (queries starting with "what", "how", "why", "when", "is", "can", "does")
2. GPT-4o generates 3-5 FAQ items from actual search queries
3. CMSService.AddFAQToPage(pageID, faqs)
4. Website already renders FAQ blocks from layout[] — no code change needed
```

**`internal/workers/content_refresher.go`** — new Asynq task `seo:refresh_content`

```
Flow:
1. Find blog posts with declining CTR (compare 30-day vs previous 30-day)
2. Fetch current Content + Paragraph from CMS
3. GPT-4o rewrites with:
   - Updated date references
   - Better keyword targeting (from current top queries)
   - Richer paragraphs (add H2s, lists)
4. CMSService.UpdatePostContent(postID, newContent, newParagraphs)
```

**`internal/workers/internal_linker.go`** — new Asynq task `seo:internal_link`

```
Flow:
1. Fetch all posts from CMS (CMSService.ListPosts)
2. Build topic clusters (group by category + query overlap)
3. Find orphan posts (no internal links pointing to them)
4. GPT-4o suggests link placements:
   - Which source post should link to which target post
   - Which paragraph index to place the link
5. CMSService.AddInternalLink(sourcePostID, paragraphIdx, targetURL, targetPostID)
```

#### 1.8 Skip Non-CMS Pages

**File:** `internal/workers/content.go` — modify `detectPageSource`

Celebrity/astrologer pages (UUID URLs) cannot be updated through CMS. Skip them during issue generation:

```go
func (s *Server) detectPageSource(pageURL string) (string, string, models.SEOContent) {
    empty := models.SEOContent{Title: pageURL}

    // Skip pages we can't update (celebrity, astrologer — UUID-based URLs)
    uuidRegex := regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-`)
    if uuidRegex.MatchString(pageURL) {
        return "skip", "", empty  // ← new: signal to skip
    }

    if s.cms == nil || !s.cms.IsConfigured() {
        return "", "", empty
    }

    target, err := s.cms.ResolveTarget(pageURL)
    if err != nil {
        return "skip", "", empty  // ← can't resolve = can't update = skip
    }

    // ... rest unchanged
}
```

Then in `handleGenerateContent`, check for "skip":

```go
pageSource, cmsPageID, current := s.detectPageSource(issue.Page)
if pageSource == "skip" {
    log.Printf("[content]   ⊘ skipping — page not manageable via CMS")
    continue
}
```

#### 1.9 Remove `updateMainAPI` Usage from ExecuteService

**File:** `internal/services/execute.go`

Since we never touch 91astro-api, simplify the execute flow:

```go
func (e *ExecuteService) ApplySEOChange(suggestion *models.SeoSuggestion) error {
    // All changes go through CMS
    if err := e.updatePayloadCMS(suggestion); err != nil {
        return fmt.Errorf("cms update: %w", err)
    }

    // Bust ISR cache
    if err := e.revalidatePage(suggestion.Page); err != nil {
        log.Printf("WARN: revalidate failed for %s: %v (non-fatal)", suggestion.Page, err)
    }
    return nil
}
```

#### 1.10 Updated Scheduler

**File:** `internal/scheduler/scheduler.go`

```go
func (s *Scheduler) Start() {
    // Daily 06:00 — ingest GSC data → auto-chains to analyze
    s.register("0 6 * * *", workers.TaskIngest, nil, "critical")

    // Sunday 09:00 — generate meta improvements for top 20 issues
    s.register("0 9 * * 0", workers.TaskGenerateContent,
        map[string]int{"top_n": 20}, "default")

    // Sunday 09:00 — track impact of live changes
    s.register("0 9 * * 0", workers.TaskTrackImpact, nil, "default")

    // Sunday 10:00 — email weekly report
    s.register("0 10 * * 0", workers.TaskReport, nil, "low")

    // NEW — Wednesday 08:00 — create blog posts for content gaps
    s.register("0 8 * * 3", workers.TaskCreateContent,
        map[string]int{"max_posts": 3}, "default")

    // NEW — Thursday 08:00 — add FAQ blocks to high-query pages
    s.register("0 8 * * 4", workers.TaskGenerateFAQ,
        map[string]int{"max_pages": 10}, "default")

    // NEW — Friday 08:00 — refresh stale blog content
    s.register("0 8 * * 5", workers.TaskRefreshContent,
        map[string]int{"max_posts": 5}, "default")

    // NEW — Saturday 08:00 — add internal links
    s.register("0 8 * * 6", workers.TaskInternalLink,
        map[string]int{"max_links": 10}, "default")

    // Keep Pipeline 2 research (Monday)
    s.register("0 8 * * 1", workers.TaskResearch,
        map[string]int{"top_n": 5}, "default")
}
```

**Weekly calendar:**

```
Monday:    Research new signals from GSC
Tuesday:   (idle — let Monday's ingest process)
Wednesday: Create 3 new blog posts for content gaps
Thursday:  Add FAQ blocks to 10 pages
Friday:    Refresh 5 stale blog posts
Saturday:  Add 10 internal links
Sunday:    Generate meta improvements + track impact + report
Daily:     Ingest GSC data at 6am
```

---

### Phase 2: Add Structured Data to Website (91astro-website)

These are one-time code changes. After this, the website automatically renders structured data from CMS content — no further website changes needed.

#### 2.1 JSON-LD Base Component

**New file:** `src/components/StructuredData/JsonLd.js`

```javascript
export default function JsonLd({ data }) {
  return (
    <script
      type="application/ld+json"
      dangerouslySetInnerHTML={{ __html: JSON.stringify(data) }}
    />
  )
}
```

#### 2.2 BlogPosting Schema on Blog Pages

**File:** `src/app/(91astrology)/[locale]/(group)/blogs/[slug]/page.js`

Add inside the `BlogPage` component, using data already fetched from CMS:

```javascript
// blogData is already fetched: GET /api/Posts/{id}?locale={locale}
// No new fetch needed — just render structured data from existing response

<script type="application/ld+json"
  dangerouslySetInnerHTML={{ __html: JSON.stringify({
    "@context": "https://schema.org",
    "@type": "BlogPosting",
    "headline": blogData.Heading,
    "description": blogData.meta?.description || blogData.Content?.[0]?.children?.[0]?.text,
    "image": `${S3_URL}/${blogData.image?.filename}`,
    "author": {
      "@type": "Person",
      "name": blogData.author?.name || "91Astrology"
    },
    "publisher": {
      "@type": "Organization",
      "name": "91Astrology",
      "logo": { "@type": "ImageObject", "url": "https://www.91astrology.com/logo.png" }
    },
    "datePublished": blogData.Date,
    "mainEntityOfPage": `https://www.91astrology.com/${locale}/blogs/${params}`
  })}}
/>
```

#### 2.3 FAQ Schema on Pages with FAQ Blocks

When a page's `layout[]` contains a block with `blockType: "FAQ"`, render FAQPage JSON-LD.

**Where to add:** In any component that renders FAQ blocks, or in the page component after extracting FAQ from layout:

```javascript
// Extract FAQ blocks from layout (already fetched from CMS)
const faqBlocks = pageData.layout?.filter(block => block.blockType === 'FAQ') || []
const faqItems = faqBlocks.flatMap(block => block.FAQ || [])

{faqItems.length > 0 && (
  <script type="application/ld+json"
    dangerouslySetInnerHTML={{ __html: JSON.stringify({
      "@context": "https://schema.org",
      "@type": "FAQPage",
      "mainEntity": faqItems.map(faq => ({
        "@type": "Question",
        "name": faq.Question,
        "acceptedAnswer": {
          "@type": "Answer",
          "text": faq.Answer?.map(a => a.Answer).join(' ')
        }
      }))
    })}}
  />
)}
```

**Key:** The website already fetches layout blocks. This just adds a `<script>` tag using that data. When the SEO agent adds FAQ blocks to a page (Phase 1.2), the website automatically renders the FAQ schema — no additional code change.

#### 2.4 Organization + WebSite Schema on Root Layout

**File:** `src/app/(91astrology)/layout.js`

Add once, applies to every page:

```javascript
<script type="application/ld+json"
  dangerouslySetInnerHTML={{ __html: JSON.stringify({
    "@context": "https://schema.org",
    "@type": "Organization",
    "name": "91Astrology",
    "url": "https://www.91astrology.com",
    "logo": "https://www.91astrology.com/logo.png",
    "description": "India's trusted Vedic astrology platform"
  })}}
/>
<script type="application/ld+json"
  dangerouslySetInnerHTML={{ __html: JSON.stringify({
    "@context": "https://schema.org",
    "@type": "WebSite",
    "name": "91Astrology",
    "url": "https://www.91astrology.com"
  })}}
/>
```

#### 2.5 BreadcrumbList Schema

Add to blog pages and zodiac pages:

```javascript
// Blog post
<script type="application/ld+json"
  dangerouslySetInnerHTML={{ __html: JSON.stringify({
    "@context": "https://schema.org",
    "@type": "BreadcrumbList",
    "itemListElement": [
      { "@type": "ListItem", "position": 1, "name": "Home", "item": "https://www.91astrology.com" },
      { "@type": "ListItem", "position": 2, "name": "Blogs", "item": `https://www.91astrology.com/${locale}/blogs` },
      { "@type": "ListItem", "position": 3, "name": blogData.Heading }
    ]
  })}}
/>
```

#### 2.6 Fix Blog Page generateMetadata

**File:** `src/app/(91astrology)/[locale]/(group)/blogs/[slug]/page.js`

Current (line 388-409):
```javascript
// BEFORE: weak metadata
title: blogData?.Heading || '91Astrology',
description: blogData?.Content?.[0]?.children?.[0]?.text || "91Astrology"
```

After (use CMS meta fields that the SEO agent populates):
```javascript
// AFTER: use meta fields from CMS (populated by SEO agent)
export async function generateMetadata({ params }) {
  let isValid = await isCategory(params?.slug);
  if (isValid) {
    const category = params?.slug?.[0].toUpperCase() + params?.slug.slice(1)
    return {
      title: `${category} Blogs | Articles on ${category} Astrology`,
      description: params?.slug
    }
  }

  let blogId = params?.slug.split('-')?.pop() || null;
  const blogData = await getBlogData(blogId, 4, params?.locale);
  if (!Array.isArray(blogData) && blogData?.errors) return notFound()

  const title = blogData?.meta?.title || blogData?.Heading || '91Astrology'
  const description = blogData?.meta?.description
    || blogData?.Content?.[0]?.children?.[0]?.text
    || '91Astrology'
  const imageUrl = blogData?.image?.filename
    ? `https://91astro-payload-cms.s3.ap-south-1.amazonaws.com/${blogData.image.filename}`
    : null

  return {
    title,
    description,
    openGraph: {
      title,
      description,
      url: `https://www.91astrology.com/${params?.locale}/blogs/${params?.slug}`,
      siteName: '91Astrology',
      type: 'article',
      ...(imageUrl && { images: [{ url: imageUrl, width: 1200, height: 630 }] }),
      publishedTime: blogData?.Date,
    },
    twitter: {
      card: 'summary_large_image',
      title,
      description,
      ...(imageUrl && { images: [imageUrl] }),
    },
  }
}
```

**Why this matters:** Once the SEO agent writes `meta.title` and `meta.description` to a Post, this code picks them up automatically. Before this change, `meta.title` from CMS was ignored — only `Heading` was used.

#### 2.7 Add OpenGraph to All Page Types

Apply the same pattern to every `generateMetadata()`. All CMS pages already have `meta.title` and `meta.description` — they're just not used for OpenGraph.

**Files to update:**
- `src/app/(91astrology)/page.js` — homepage
- `src/app/(91astrology)/[locale]/(group)/[zodiac]/page.js` — zodiac pages
- `src/app/(91astrology)/[locale]/(group)/horoscope/[slug]/page.js` — horoscope pages
- `src/app/(91astrology)/[locale]/(group)/blogs/page.js` — blog listing

Pattern for each:
```javascript
const imageUrl = "https://www.91astrology.com/og-default.png" // or from CMS

return {
  title: product?.meta?.title || 'Page Title',
  description: product?.meta?.description || '',
  openGraph: {
    title: product?.meta?.title,
    description: product?.meta?.description,
    siteName: '91Astrology',
    type: 'website',
    images: [{ url: imageUrl }],
  },
  twitter: {
    card: 'summary_large_image',
    title: product?.meta?.title,
    description: product?.meta?.description,
  },
}
```

#### 2.8 Dynamic Sitemap

**New file:** `src/app/sitemap.js`

Replace static XMLs in `/public/` with Next.js dynamic sitemap:

```javascript
const BASE_URL = 'https://www.91astrology.com'
const CMS_URL = process.env.BASE_URL
const LOCALES = ['en', 'hi', 'ta', 'bn', 'es', 'gu', 'ka', 'mr']

export default async function sitemap() {
  const entries = []

  // Static pages
  const staticPages = ['', '/blogs', '/horoscope', '/janam-kundli',
    '/kundli-matching', '/numerology', '/celebrity', '/store']

  for (const page of staticPages) {
    entries.push({
      url: `${BASE_URL}${page}`,
      lastModified: new Date(),
      changeFrequency: page === '' ? 'daily' : 'weekly',
      priority: page === '' ? 1.0 : 0.8,
    })
  }

  // Zodiac pages
  const signs = ['aries','taurus','gemini','cancer','leo','virgo',
    'libra','scorpio','sagittarius','capricorn','aquarius','pisces']
  for (const sign of signs) {
    entries.push({
      url: `${BASE_URL}/${sign}`,
      lastModified: new Date(),
      changeFrequency: 'daily',
      priority: 0.9,
    })
  }

  // Blog posts from CMS
  try {
    const res = await fetch(`${CMS_URL}/api/posts?limit=500&depth=0`)
    const data = await res.json()
    for (const post of data.docs || []) {
      if (post.isHidden) continue
      entries.push({
        url: `${BASE_URL}/blogs/${post.slug}-${post.id}`,
        lastModified: new Date(post.updatedAt || post.createdAt),
        changeFrequency: 'monthly',
        priority: 0.7,
      })
    }
  } catch (e) {
    console.error('Sitemap: failed to fetch posts', e)
  }

  return entries
}
```

#### 2.9 Reduce ISR to 1 Hour

**File:** `src/config/loadAppConfig.js`

```javascript
// Change:
export const REVALIDATE = 172800  // 48 hours
// To:
export const REVALIDATE = 3600    // 1 hour
```

CMS changes from the SEO agent will appear within 1 hour instead of 2 days.

---

## How It All Connects After Implementation

```
 SEO agent writes to CMS              Website reads from CMS
 ═══════════════════════               ═══════════════════════

 UpdatePostMeta(id, title, desc)  →    generateMetadata() reads meta.title
                                       → <title>Aries Horoscope Today 2026</title>
                                       → <meta name="description" content="...">
                                       → <meta property="og:title" content="...">

 AddFAQToPage(id, faqs)          →    layout[] has FAQ block
                                       → renders FAQ accordion (existing)
                                       → renders FAQPage JSON-LD (Phase 2.3)
                                       → Google shows FAQ rich snippet

 CreatePost({Heading, Content})  →    /api/posts returns new post
                                       → blog listing shows it
                                       → sitemap.js includes it
                                       → BlogPosting JSON-LD renders (Phase 2.2)
                                       → Google indexes new content

 UpdatePostContent(id, content)  →    blog page re-renders with richer text
                                       → more keywords → more long-tail matches

 AddInternalLink(id, idx, url)   →    Paragraph[idx].Hyperlink renders as <a>
                                       → passes link equity to target page
                                       → target page ranks higher
```

---

## Execution Order (Priority)

| # | Task | Repo | Impact | Effort |
|---|------|------|--------|--------|
| 1 | **2.4** Organization + WebSite schema on root layout | website | High — every page gets schema | 10 min |
| 2 | **2.2** BlogPosting schema on blog pages | website | High — blog rich snippets | 30 min |
| 3 | **2.6** Fix generateMetadata to use meta.title + OpenGraph | website | High — better CTR + social sharing | 1 hr |
| 4 | **2.3** FAQ schema on pages with FAQ blocks | website | High — FAQ rich snippets | 30 min |
| 5 | **2.5** BreadcrumbList schema | website | Medium — breadcrumb snippets | 30 min |
| 6 | **2.9** Reduce ISR to 1 hour | website | Medium — faster content updates | 5 min |
| 7 | **2.8** Dynamic sitemap | website | Medium — better crawl discovery | 1 hr |
| 8 | **2.7** OpenGraph on all page types | website | Medium — social sharing | 1 hr |
| 9 | **1.8** Skip non-CMS pages (celebrity/astrologer) | seo | Fix — stops 404 errors on approve | 15 min |
| 10 | **1.9** Remove updateMainAPI from execute flow | seo | Fix — CMS-only execution | 15 min |
| 11 | **1.2** AddFAQToPage method | seo | High — FAQ blocks via API | 1 hr |
| 12 | **1.7a** FAQ generator worker | seo | High — auto-generates FAQs from GSC queries | 2 hr |
| 13 | **1.3** CreatePost method | seo | High — new blog content | 1 hr |
| 14 | **1.7b** Content creator worker | seo | High — auto-creates blog posts | 3 hr |
| 15 | **1.5** AddInternalLink method | seo | Medium — link equity | 1 hr |
| 16 | **1.7d** Internal linker worker | seo | Medium — automated linking | 2 hr |
| 17 | **1.4** UpdatePostContent method | seo | Medium — content enrichment | 30 min |
| 18 | **1.7c** Content refresher worker | seo | Medium — refresh stale posts | 2 hr |
| 19 | **1.6** ListPosts method | seo | Foundation for content analysis | 30 min |
| 20 | **1.10** Updated scheduler (weekly calendar) | seo | Automation — runs everything on schedule | 30 min |

**Total: ~20 hours of implementation**

**Expected impact:**
- Tasks 1-5 (structured data): 2-5x rich snippet appearances within 2-4 weeks
- Tasks 6-8 (website fixes): Faster indexing + social traffic
- Tasks 9-10 (agent fixes): Stop errors, CMS-only execution
- Tasks 11-20 (agent expansion): Continuous content improvement, compounding traffic growth
