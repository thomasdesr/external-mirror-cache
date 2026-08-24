# Vary races on URL-only cache keys

Status: hole 1 and the flap are fixed by Accept-variant cache keys on
every path (docs/design-plans/2026-08-20-accept-variant-keys.md), which
give negotiating URLs per-variant objects and per-variant singleflight
groups. Hole 2 (the mutable-key redirect) is deliberately shelved: with
variants keyed apart, no remaining same-key overwrite can hand a client
another variant's bytes. What remains is byte-identical re-uploads
(content-addressed registry paths whose upstreams provide no strong
validator to match), deliberately-rotating responses (auth tokens), and
genuine same-resource updates (an index page after a release) — a
racing client gets the same resource, possibly a moment newer, which is
the ordinary CDN experience. So version-pinned presigned URLs (the
first fix direction below, validated in a prototype) are deliberately
not in the tree rather than pay for bucket versioning, which would also
store a full-size version per touch and per byte-identical re-upload.
Its trigger: reports of a client receiving harmfully wrong bytes — a
different resource or another client's variant, not merely a newer copy
of what it asked for. Hole 3 (upstream `Vary`-blind conditional
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
- **Per-variant keys** close hole 1 at the source: distinct variants
  become distinct S3 objects and distinct singleflight groups. Shipped
  as Accept-variant keys on every path; rationale, accepted costs, and
  the normalization fallback (a per-protocol adapter mapping Accept
  into a spec-defined representation enum, if per-URL Accept diversity
  ever grows) live in
  docs/design-plans/2026-08-20-accept-variant-keys.md.
- **Hole 3 needs no mechanism** until an origin in the traffic actually
  exhibits it; the freshness touch already refuses validator-mismatched
  updates, which is the same class of defense on the metadata side.

## Trigger

For hole 2 (version pinning): reports of a client receiving harmfully
wrong bytes, per the Status above. Version pinning can also ride any
unrelated storage-layer change if bucket versioning gets enabled for
other reasons.

## Adjacent, accepted: encoded-slash collapse

`parseTargetURL` builds the upstream URL from the decoded request path,
so `%2F` collapses to `/` before the key or the fetch is formed: a
request for a path containing an encoded slash silently maps to the
resource at the slash-split path. This sits one layer above the key
grammar (which is injective over what the parser hands it) and is
unreachable in current traffic — no upstream in the mirror's host set
uses `%2F` as a path literal. Fix shape if one appears (GitLab-style
APIs are the known example): parse the target from the raw escaped path
instead of the decoded one.
