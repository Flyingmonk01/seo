---
name: no-api-changes
description: Never modify 91astro-api repo. Only changes in 91astro-seo and 91astro-website (and 91astro-cms).
type: feedback
---

Never make changes to 91astro-api (NestJS). All work is scoped to:
- 91astro-seo (Go agent)
- 91astro-website (Next.js frontend)
- 91astro-cms (Payload CMS)

**Why:** User explicitly stated 91astro-api is off-limits. The SEO system must work without any API backend changes.

**How to apply:** For pages that currently route to `PageSourceAPI` (celebrity, astrologer pages with UUIDs), either skip them or route through CMS instead. Remove any code paths that depend on 91astro-api internal endpoints.
