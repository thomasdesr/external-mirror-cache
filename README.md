# external-mirror-cache

An HTTP caching proxy that stores upstream responses in S3 and serves cache hits as presigned URL redirects. Designed for internal infrastructure that repeatedly fetches the same external files (package repositories, container images, release artifacts).

## Problem

Services that download external resources — OS packages during instance boot, language dependencies in CI, binary artifacts in deploy pipelines — make redundant requests to the same URLs. This creates several issues:

- **Reliability:** A transient upstream outage (CDN blip, rate limit, DNS failure) breaks builds and deploys that would otherwise succeed with cached content.
- **Latency:** Every request pays full round-trip cost to the origin, even when the content hasn't changed.
- **Bandwidth:** The same multi-megabyte files get pulled across the internet repeatedly.

Existing solutions either require client-side configuration changes (explicit proxy settings, alternate registry URLs) or run a full mirror that needs its own sync schedule and storage management.

## Design

The proxy sits behind an internal load balancer and requires no client-side proxy configuration — clients just point their base URL at it.

**URL scheme:** `GET /<domain>/<path>` maps to `https://<domain>/<path>`. A request to `/example.com/releases/v1.0.tar.gz` fetches `https://example.com/releases/v1.0.tar.gz`.

**Request flow:**

1. Check S3 for a cached copy (HeadObject to read stored response headers).
2. If cached, send a conditional request upstream with `If-None-Match` / `If-Modified-Since`.
3. On **304 Not Modified**: redirect the client to a presigned S3 URL for the cached object.
4. On **200 OK**: stream the response body to S3 via the transfer manager, then redirect to the presigned URL.
5. On **upstream failure**: optionally serve stale cached content based on a configurable fallback policy (connection errors, 5xx responses, or any error).

Concurrent requests for the same URL are deduplicated via singleflight — only one request hits upstream, and all waiters receive the same presigned URL.

**Key properties:**

- **No client-side proxy config.** Clients use normal HTTP GETs with a rewritten base URL.
- **Conditional requests.** ETag and Last-Modified headers are stored as S3 object metadata and used for revalidation, so unchanged content is never re-downloaded or re-uploaded.
- **Stale-serving fallback.** When upstream is unavailable, previously cached content can still be served. Controlled per failure class (connection errors, 5xx, any error).
- **Redirect-based serving.** Clients follow a 303 to a presigned S3 URL, so the proxy never buffers cached responses through its own process.
- **Upstream redirect following.** The proxy follows redirects before caching. The cache key is the original requested URL, not the final redirect destination.
- **Systemd integration.** Supports socket activation and `sd_notify` for zero-downtime deploys.

## SSRF protection

Because the proxy accepts a domain in the request path and makes outbound requests to it, it is an SSRF vector by construction. An attacker who can reach the proxy could request `/<internal-host>/secrets` and have the proxy fetch it on their behalf.

The `--egress-proxy` flag addresses this by routing all upstream requests through an HTTP CONNECT proxy that enforces egress policy. The proxy only affects upstream fetches — AWS SDK traffic (S3, IMDS) uses the default transport and is unaffected.

```
mirror-cache --egress-proxy http://127.0.0.1:4750
```

[Smokescreen](https://github.com/stripe/smokescreen) is a good fit here. In its default configuration it denies connections to private/internal IP ranges while allowing public internet, which is exactly the policy this proxy needs. Any CONNECT proxy that blocks RFC 1918 and link-local addresses works — Smokescreen is just one option. The important thing is that the proxy never makes direct outbound connections without egress filtering.
