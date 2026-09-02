import type { MetadataRoute } from "next";

// This fork has no public marketing pages: "/" redirects to /login (see
// proxy.ts) and everything else is the authenticated app. Upstream allowed
// the landing routes and advertised a sitemap for multica.ai; here nothing
// should be indexed, so the whole site is disallowed and no sitemap exists.
export default function robots(): MetadataRoute.Robots {
  return {
    rules: [{ userAgent: "*", disallow: ["/"] }],
  };
}
