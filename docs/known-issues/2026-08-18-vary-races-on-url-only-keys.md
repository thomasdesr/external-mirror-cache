# Vary races on URL-only cache keys

Status: hole 1 and the flap are fixed by Accept-variant cache keys on
every path (docs/design-plans/2026-08-20-accept-variant-keys.md), which
give negotiating URLs per-variant objects and per-variant singleflight
groups. Hole 2 (the mutable-key redirect) is deliberately shelved: with
variants keyed apart, the same-key overwrites that remain are
byte-identical re-uploads (content-addressed registry paths whose
upstreams provide no strong validator to match) and
deliberately-rotating responses (auth tokens) — neither can hand a
client wrong bytes — so version-pinned presigned URLs (the first fix
direction below, validated in a prototype) are deliberately not in the
tree rather than pay for bucket versioning, which would also store a
full-size version per touch and per byte-identical re-upload. Its
trigger: client wrong-bytes reports, or a content class whose bytes
genuinely change under one key. Hole 3
(upstream `Vary`-blind conditional
handling) remains accepted with no mechanism, per the trigger below.
Originally captured 2026-08-18 after watching a `/simple/` index page's
stored object alternate between its JSON and HTML representations under
mixed client Accepts.

## Shape

A `CacheKey` without a variant maps one URL to one S3 object. For URLs
whose upstream negotiates on `Accept` (responses carry `Vary: Accept` —
pypi.org's `/simple/` pages are the live example), that one object holds
whichever representation was fetched last. Sequentially this is correct
but wasteful: every request revalidates with its own Accept, upstream
answers 200 with the right body whenever the stored variant is the wrong
one, and the object flips with a full re-upload each time.

Under concurrency the same shape produces wrong bytes, three ways:

1. **Singleflight coalesces across Accepts.** The dedup group is keyed
   on `CacheKey.String()`, which for URL-only keys is the URL alone. Two
   clients hitting the same URL concurrently with different Accepts
   coalesce into one flight; the leader's Accept goes upstream and both
   clients are redirected to the leader's variant. The waiter receives a
   representation it did not ask for. This is structural
   (`fetchAndCache` takes one Accept per flight), not a timing fluke.
2. **The presigned redirect points at a mutable key.** We `Put`, then
   hand the client a presigned URL for the key — not for the object
   version we just validated or wrote. Another request (or another
   instance; singleflight is per-instance) can flip the object between
   the redirect and the client's S3 GET, and the client downloads the
   flipped bytes.
3. **Sequential correctness leans on upstream honoring `Vary` in its
   conditional handling.** A sloppy origin that answers 304 from
   `If-Modified-Since` alone, without checking that the selected
   representation matches, would pin the wrong variant even without
   concurrency. The origins in our traffic handle this correctly today.

## Bounds

Only URLs that actually negotiate are exposed — in our traffic, index
pages, never artifacts (wheels and blobs carry no `Vary`; OCI manifests
are already variant-keyed, which gives them per-variant S3 objects and
per-variant singleflight groups). A wrong-variant index page fails loud
on the client (unexpected content type, parse error, retry) rather than
silently corrupting anything, and clients that verify artifact hashes
are unaffected end to end. The freshness gate's Vary guard keeps the
fast path out of this entirely: a `Vary`-mismatched entry is never
served fresh, so the gate never freezes a flapped object.

## Fix directions

- **Version-pinned presigned URLs** close hole 2 on their own: presign
  `GetObject` with the `VersionId` of the object the decision was made
  against (HeadObject returns it on the fresh path; PutObject and
  CopyObject return it on the write paths). Requires bucket versioning,
  which brings a lifecycle rule for noncurrent-version expiry and its
  storage cost; a touch's CopyObject also creates a new version, though
  presigning the pre-touch version stays byte-identical. This also
  hardens the artifact path against any future overwrite race, not just
  Vary flapping.
- **Per-variant keys via a protocol adapter** close holes 1 and 2 for
  the negotiating URLs at the source: distinct variants become distinct
  S3 objects and distinct singleflight groups, exactly as OCI manifests
  work today. The right scope is a PyPI-simple adapter keying on the
  spec-defined media types — the Accept values there are exact variant
  names, not preference lists to interpret. Generic Vary-driven keying
  is not a candidate: which request headers matter is response
  knowledge, and up-front keying by Accept would rekey (and orphan) the
  URL-only corpus while fragmenting non-negotiating URLs per client
  Accept string.
- **Hole 3 needs no mechanism** until an origin in the traffic actually
  exhibits it; the freshness touch already refuses validator-mismatched
  updates, which is the same class of defense on the metadata side.

## Trigger

Act when either: wrong-variant reports (client parse errors on index
pages correlating with mixed-Accept concurrency), or `/simple/`
revalidation volume grows to matter — the adapter then fixes waste and
correctness together. Version pinning can ride any unrelated
storage-layer change earlier if versioning gets enabled for other
reasons.
