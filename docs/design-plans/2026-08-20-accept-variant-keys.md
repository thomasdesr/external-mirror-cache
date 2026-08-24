# Accept-Variant Cache Keys on Every Path

## Summary

Every cache entry is keyed by the request's complete Accept set, not just
OCI manifest requests: same URL, different Accept, different S3 object and
different singleflight group. Requests without an Accept keep URL-only
keys. This generalizes the OCI variant mechanism into the cache's one
keying rule and retires the Vary races that URL-only keys carried on
negotiating URLs.

## Problem

With URL-only keys, a URL whose upstream negotiates on Accept (responses
carry `Vary: Accept` — pypi.org's `/simple/` pages are the live example)
holds whichever representation was fetched last:

- Sequentially, the entry flaps: each request revalidates with its own
  Accept, the stored validators belong to the other variant, so the
  conditional never matches, upstream answers a full 200, and the body is
  re-uploaded. Correct bytes, constant churn, and the etag-skip can never
  fire across variants.
- Concurrently, it serves wrong bytes: singleflight coalesces requests by
  key, so two clients with different Accepts share one flight and the
  waiter is redirected to the leader's variant.
- Under the freshness gate, the Vary guard (correctly) refuses these
  entries, so negotiating hosts are excluded from fresh serving entirely.

## Decision

Key by the raw Accept string, on every path.

- **Raw ask, not response knowledge.** Which headers actually select the
  representation is revealed only by the response's `Vary`, after a key
  has already been chosen; and whether two different Accepts are
  equivalent is decided by the origin's negotiation logic, which no cache
  can compute. Keying on the raw ask over-partitions — a conservative
  error that duplicates but can never serve one client's variant to
  another.
- **Over-partitioning is cheap for CI-shaped traffic.** The clients are
  build tools, and build tooling speaks a small, stable set of Accept
  idioms: artifact fetchers ask with a wildcard, negotiating clients
  (pip's simple API, OCI pulls) ask with exact media types. Distinct
  Accepts on the same URL are the exception, so raw-Accept keying
  duplicates little — and duplication is the failure mode, never wrong
  bytes.
- **No hashing.** A hash is a different spelling of the same partition; it
  merges nothing raw strings would split. Raw keeps the S3 key readable.
  A pathological multi-kilobyte Accept would push the key past S3's
  length cap and fail loudly on every attempt for that request shape;
  no real client tooling sends one, so no bound is built — hashing
  oversized variants is the fix if such a client ever appears.
- **Accept-less requests keep URL-only keys and make no Accept-encoded
  claim.** URL-only keys are where the pre-variant corpus lives; entries
  stored there by clients that did send Accepts must not pass the
  freshness Vary guard. Accepted cost: an Accept-less client on a
  `Vary: Accept` host revalidates on every request, forever — correct,
  just never fast; no known client omits Accept.
- **The S3 key grammar is injective, via stdlib encoders only.** A key
  is `prefix "/" PathEscape(host) EscapedPath ["?" QueryEscape(query)]
  ["??" PathEscape(variant)]`. Every encoder's output excludes a raw
  `?`, so every `?` in a finished key is a structural joiner: `??`
  marks the variant, the first remaining `?` marks the query. Without
  this, raw request paths could spell the joiners themselves: a path
  ending `//<token>` shared an object with the bare path plus
  `Accept: token`, and a path containing a literal `?` (paths arrive
  decoded, so `%3F` produces one) shared an object with the same path
  split at the `?` into path-plus-query — two distinct resources on one
  key, whichever wrote last serving its bytes for both. The encoders
  are the identity on real hostnames and real paths, so keys stay
  byte-identical to the corpus, read exactly like the URLs they cache,
  and keep the `/` hierarchy for prefix listing; a URL's variant
  objects sort beside their URL-only twin.

## Consequences

- The existing corpus is orphaned: every entry fetched with an Accept
  re-keys and re-downloads on first request. Accepted deliberately —
  correctness first; the dead URL-only objects can be garbage-collected
  later.
- Client Accept drift re-keys that client's URL slice when it happens,
  bounded by how rarely CI tooling changes its Accept idiom.
- `Vary: Accept` hosts join the freshness fast path: their entries pass
  the Vary guard, revalidate once per fresh window, and stop flapping.
- The OCI blob fragmentation wart (content-addressed `/blobs/` paths
  fragmented per Accept string) now applies to any path — duplicate
  bytes, never wrong bytes.
- A scoped per-protocol adapter that normalizes Accept into a
  spec-defined representation enum remains the fallback if traffic ever
  grows real per-URL Accept diversity on large artifacts.

## Alternatives

- **Response-driven Vary keying** (store Vary, second lookup keyed by the
  named request headers): an extra storage round trip on every request
  plus a primary/variant consistency surface, to collapse duplicates
  that CI-shaped traffic rarely produces. Deferred with the adapter.
- **Do nothing**: leaves negotiating URLs flapping, cross-variant
  validators broken, and the coalescing wrong-bytes race open
  (docs/known-issues/2026-08-18-vary-races-on-url-only-keys.md).
