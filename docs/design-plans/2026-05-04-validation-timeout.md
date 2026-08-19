# Validation Timeout Configuration

## Problem

When upstream becomes unreachable (e.g., the Canonical apt outage that motivated this
change), the proxy waits ~50s before falling back to cached content. The shared
transport in `main.go:97-101` configures a 10s `Dialer.Timeout`, 10s
`TLSHandshakeTimeout`, and 30s `ResponseHeaderTimeout`; those add up to roughly the
total budget a hung connection consumes before `client.Do` (`server.go:116`) returns
an error. Only after that error does `fetchAndCache` enter the fallback branch
(`server.go:118-122`) and serve stale via `presign`. The total can exceed ALB target
response budgets (60s) and Bazel per-fetch timeouts, so the slow path turns into a
client-visible failure even though we have the bytes in S3. The healthy validation
path is fast (~1s for a 304), so a much tighter budget on the conditional-fetch path
is safe.

## Goals & Non-goals

- Goals:
  - Enter the existing fallback branch (`server.go:118-122`) within a single-digit
    seconds budget when upstream is unreachable AND we have cached content.
  - Keep healthy 304 validations succeeding well within the new budget on a normal
    upstream.
  - Lower the overall `client.Timeout` (`main.go:109`) from 5 minutes to a value that
    keeps multi-MB `.deb` cache misses working while still bounding worst-case latency.
  - Expose every new timeout as a flag with env-var fallback, matching the existing
    `flag.String`/`envDefault` pattern in `main.go:37-50`.
- Non-goals:
  - **Stale-while-revalidate.** Already considered and rejected upstream of this
    design: it changes client semantics for mutable content (apt indexes, OCI tags),
    and the proxy's `CLAUDE.md` policy is "validate with conditional requests" before
    serving. We preserve that in the healthy path.
  - Upstream health probing or circuit breaking. `CLAUDE.md` "Out of Scope" explicitly
    excludes upstream health monitoring.
  - Changing the fallback policy itself (`fallback.go`); it already does the right
    thing once the error surfaces (`server.go:118`).
  - Connection pooling / retry policy changes. Out of scope for the timeout fix.

## Alternatives Considered

1. **Per-request `context.WithTimeout` on the conditional-fetch path (chosen).**
   In `fetchAndCache` (`server.go:90-125`), when `conditionalFetch == true`
   (`server.go:100`), wrap the request context with a tight deadline before calling
   `m.client.Do`. The deadline bounds the entire request: dial, TLS, headers, and
   body for the 304 case (which has no body). On miss (`conditionalFetch == false`)
   we fall through to the existing `client.Timeout`. Chosen because it requires no
   new transport, no new client, and naturally interacts with the existing detached
   context (`server.go:65-69`) without needing to thread state through singleflight.

2. **Dedicated second `*http.Client` and `*http.Transport` for validation.**
   Build a parallel transport with `DialContext` timeout 2s, `TLSHandshakeTimeout`
   2s, `ResponseHeaderTimeout` 3s, and a separate `http.Client.Timeout` ~5s. Route
   conditional fetches through it. Rejected: doubles connection-pool footprint
   (validation and miss paths can't share idle conns), bypasses `ociAuthTransport`
   unless we wrap twice, and the timeout split between transport and client is
   harder to reason about than a single context deadline. Worth revisiting only if
   we ever want different proxy/TLS settings per path.

3. **Tighten the shared transport's dial / TLS / response-header timeouts.**
   Drop `DialContext.Timeout` to 3s, `TLSHandshakeTimeout` to 3s,
   `ResponseHeaderTimeout` to 5s in `main.go:97-101`. Rejected as the primary
   mechanism: those timeouts are transport-wide, so they would also clamp the
   cache-miss path where a slow-but-progressing upstream legitimately spends
   seconds in TLS or many seconds emitting a streamed body. We do recommend a
   modest tightening of `ResponseHeaderTimeout` (see Recommended Design) because
   30s is already well past any healthy upstream, but the tight validation budget
   must come from the per-request context.

4. **Stale-while-revalidate (rejected upstream, listed for completeness).**
   Serve presigned URL immediately on cache hit, kick off background revalidation.
   Rejected for client-semantic reasons (apt/OCI mutable content); see Non-goals.

5. **Negative-result memoization of recent validation failures.**
   If a validation just failed, short-circuit subsequent validations to the
   fallback path for some TTL. Listed under Open Questions; out of scope here.

## Recommended Design

Add a per-request deadline on the conditional-fetch path and modestly lower the
overall client timeout. No new transport or client.

Concrete changes:

1. **New flag-driven config** (parsed in `main.go` near lines 37-50):
   - `--validation-timeout` / `MIRROR_CACHE_VALIDATION_TIMEOUT` — duration applied
     only when `conditionalFetch == true` in `fetchAndCache` (`server.go:100`).
   - `--client-timeout` / `MIRROR_CACHE_CLIENT_TIMEOUT` — replaces the hardcoded
     `5 * time.Minute` at `main.go:109`.
   - `--response-header-timeout` / `MIRROR_CACHE_RESPONSE_HEADER_TIMEOUT` —
     replaces the hardcoded `30 * time.Second` at `main.go:101`. Exposing this lets
     operators tune the cache-miss ceiling without a code change; default is a
     mild tightening from 30s to 20s.
   - Existing dial timeout (`main.go:97-99`) and `TLSHandshakeTimeout`
     (`main.go:100`) remain at 10s each, transport-wide. They are not the dominant
     contributor on the conditional path once `validation-timeout` is enforced via
     context, because the context deadline cancels the in-flight dial / TLS
     handshake too.

2. **Plumb `validationTimeout` into `cacheMiddleware`.**
   Add a `validationTimeout time.Duration` field on `cacheMiddleware`
   (`server.go:32-38`) wired from `main.go` alongside `cache`, `client`, `fallback`,
   `keyFunc`. Inside `fetchAndCache` (`server.go:90`), after the
   `conditionalFetch` boolean is established (`server.go:100`) and before
   `buildUpstreamRequest` (`server.go:109`), if `conditionalFetch && m.validationTimeout > 0`
   derive a NEW context variable (e.g. `reqCtx`) via
   `context.WithTimeout(ctx, m.validationTimeout)` and defer its cancel. Pass
   `reqCtx` into `buildUpstreamRequest` so `http.NewRequestWithContext`
   (`server.go:170`) carries the deadline through `m.client.Do`
   (`server.go:116`). See "Implementation contract" below for which calls
   must continue to use the original `ctx`.

3. **Fallback wiring is already correct.**
   When the deadline fires mid-request, `client.Do` returns a `*url.Error` whose
   inner error is a `net.Error` with `Timeout() == true`. `isConnectionError` in
   `fallback.go:35-39` matches via `errors.As(err, &netErr)`, so
   `ShouldFallback(err, 0)` returns true under the default
   `OnConnectionError: true`, and `server.go:118-122` serves stale via `presign`.
   No changes required to `fallback.go` or `http_caching.go`.

4. **Cache-miss path unchanged in spirit, ceiling lowered.**
   When `conditionalFetch == false` we don't apply `validationTimeout`; the request
   is bounded by `client.Timeout` (the new `--client-timeout` flag). This preserves
   the ability to stream large `.deb` bodies. The new default is 120s, which is
   still well above the typical multi-MB transfer time over a healthy link.

5. **Logging.** When the validation deadline fires, the existing
   `server.go:119` warn line ("upstream response", `action=stale`,
   `action_reason=err.Error()`) prints `context deadline exceeded`. That's
   sufficient for operators; no new log fields proposed.

### Implementation contract

The validation deadline applies ONLY to the upstream HTTP request issued by
`m.client.Do` (`server.go:116`). Every other call site in `fetchAndCache` and
its helpers MUST continue to use the original, un-deadlined `ctx` parameter.
Concretely:

- `m.cache.Head(ctx, key)` at `server.go:93` — uses original `ctx`. The cache
  Head happens before the deadline is derived; this is structural, but call it
  out so the implementor doesn't refactor it under the deadline by accident.
- `m.client.Do(req)` where `req` is built from `reqCtx` in
  `buildUpstreamRequest` (`server.go:109-116`, `server.go:170`) — uses the
  derived `reqCtx`. This is the only place the validation deadline binds.
- `m.cache.Put(ctx, key, resp.Header, bufio.NewReader(resp.Body))` at
  `server.go:155` — call it with the original `ctx`. (See the
  "Refresh-during-deadline behavior" note below for what this does and does
  NOT buy us.)
- `m.presign(ctx, key)` at `server.go:121`, `:133`, `:141`, `:166` (defined at
  `server.go:186`) — uses original `ctx` in all four call sites. The presign
  must not inherit the validation deadline; the stale-fallback path
  (`server.go:121`) in particular is reached precisely because validation
  failed, and presigning S3 URLs after that point should not be racing the
  same expired deadline.

Implementation guidance: do NOT shadow `ctx` with `:=`. Use a distinct
variable name (`reqCtx`) for the deadlined context so the original remains in
scope and visibly available for the calls listed above.

### Refresh-during-deadline behavior

When upstream returns 200 (content changed) on the conditional path, the
response body is streamed to S3 by `bufio.NewReader(resp.Body)` via
`m.cache.Put` at `server.go:155`. Even though `cache.Put` is called with the
original `ctx` (per the contract above), the body reader itself is bound to
the request's `reqCtx` — `net/http` cancels in-flight body reads when the
request context's deadline fires. So a 200-OK refresh whose body takes longer
than `validation-timeout` to stream WILL fail with a deadline error during
`cache.Put`.

This is an accepted tradeoff, not a bug:

- 200 responses on the conditional path are the rare case; most validations
  return 304 (no body).
- If the refresh genuinely needs more than 5s, the upstream is slow enough
  that serving the existing stale entry and refreshing on a later request is
  preferable to making the current client wait.
- A failed refresh leaves the previously cached entry intact in S3; the next
  request will revalidate and either succeed (304) or refresh under a new
  deadline.

Do NOT introduce a "cancel on first byte" pattern (call `cancel()` after
`m.client.Do` returns a non-304 to fall back to `client.Timeout` for the body
read). It's tempting and would preserve refreshes on slow links, but it adds
a second timeout regime to reason about and the win is small. If operational
data later shows refresh failures are common and harmful, revisit then.

### Configuration interface

| Flag                          | Env var                                | Default | Purpose                                                                                       |
|-------------------------------|----------------------------------------|---------|-----------------------------------------------------------------------------------------------|
| `--validation-timeout`        | `MIRROR_CACHE_VALIDATION_TIMEOUT`      | `5s`    | Per-request deadline on the conditional-fetch path (cache-hit revalidation).                  |
| `--client-timeout`            | `MIRROR_CACHE_CLIENT_TIMEOUT`          | `120s`  | Overall `http.Client.Timeout`, replaces the hardcoded 5m at `main.go:109`. Bounds cache-miss. |
| `--response-header-timeout`   | `MIRROR_CACHE_RESPONSE_HEADER_TIMEOUT` | `20s`   | Transport-wide ceiling on time-to-first-byte, replaces hardcoded 30s at `main.go:101`.        |

A value of `0` for `--validation-timeout` disables the per-request deadline (escape
hatch). Negative values are rejected at flag-parse time.

### Timeout budget reasoning

ALB target response default is 60s; Bazel's per-fetch read timeout is in the same
neighborhood. The dominant time during the Canonical outage was ~50s of dial + TLS
+ response-header wait, well above ALB's tolerance for a single hop in a chain
that still needs to do an S3 presign and return a redirect.

A healthy 304 validation against a reachable upstream completes in ~100-300ms
including TLS resume; CDN-fronted upstreams (Canonical, Docker Hub) typically
return 304 in <1s even from a cold idle pool. A `--validation-timeout` default
of **5s** gives a 5-15x healthy-case headroom while keeping outage latency well
under 10s end-to-end (5s validation + sub-second presign + redirect). That leaves
the ALB budget mostly intact for downstream retries and Bazel's read-timeout
envelope unstressed.

`--client-timeout` of **120s** is the upper bound on cache-miss transfers. A
20MB `.deb` over 1 MB/s of egress bandwidth is 20s; over 100KB/s (worst-case for
constrained egress) it's 200s, but at that point the request is already an
operational anomaly and 120s is a reasonable hard ceiling. The previous 5-minute
value was a "never trips" placeholder; 120s bounds worst-case while still
exceeding any plausible healthy transfer.

`--response-header-timeout` of **20s** is a mild tightening from 30s. Healthy
upstreams emit response headers in well under 1s; 20s tolerates one DNS retry +
TLS renegotiation worst case while clamping pathological cache-miss waits before
`client.Timeout` kicks in. Operators wanting the old 30s can set the env var.

## Test Plan

- Unit:
  - **`TestFetchAndCache_ValidationTimeout_FallsBackToStale`** in `server_test.go`:
    seed `fakeCache` with a cached entry, point the proxy at an upstream
    `httptest.Server` whose handler blocks on `<-time.After(longerThanTimeout)`,
    construct the `cacheMiddleware` with `validationTimeout: 100ms`, assert that
    the proxy responds with 303 (stale fallback) within ~200ms. Closes the gap
    that `TestIntegration_FallbackOnConnectionError_WithCache`
    (`server_test.go:1349`) leaves: it kills the upstream so the dial fails fast,
    but doesn't exercise a hung-but-listening upstream which is the actual outage
    shape.
  - **`TestFetchAndCache_ValidationTimeout_NoCacheNoFallback`**: same hung
    upstream, empty cache; assert the proxy waits up to `client.Timeout` (use a
    small override like 500ms) and returns 502 — the validation timeout must
    NOT apply when there's no cached content to fall back to. Verifies the
    branching at `server.go:100`.
  - **`TestFetchAndCache_ValidationTimeout_HealthyPath`**: cached entry, fast
    upstream returning 304; assert success and that the request's deadline
    propagated (use `httptest.Server` handler that asserts `r.Context().Done()`
    has a deadline within the validation budget).
  - **`TestFetchAndCache_ValidationTimeout_ZeroDisablesDeadline`**: cached
    entry, slow-but-eventually-responding upstream that sleeps ~200ms and
    returns 304. Construct `cacheMiddleware` with `validationTimeout: 0`.
    Assert the request succeeds with a 303 redirect rather than tripping any
    deadline, and that the upstream handler observed an
    `r.Context().Deadline()` of `(time.Time{}, false)` (no deadline). Verifies
    the `m.validationTimeout > 0` guard documented in the Recommended Design
    and the escape-hatch contract for the env var.
  - **Flag parsing** in `main_test.go`: extend the existing parsing tests to
    cover the three new flags including env-var fallback and rejection of
    negative durations. Mirror the style of `TestLogLevelParsing`
    (`main_test.go:12`).

- Integration:
  - The existing `TestIntegration_FallbackOnConnectionError_WithCache`
    (`server_test.go:1349`) continues to pass unchanged — it tests the dial-fails
    case, which is orthogonal to the new context deadline.
  - Add an integration test using a `net.Listen` socket that accepts but never
    writes (a true black-hole upstream); verify end-to-end that the proxy
    returns 303 within `validation-timeout + small_slack`.

- Manual:
  - Run the binary against a controlled upstream (e.g., `nc -l` on a port with
    no response) with `--validation-timeout=2s` and a primed cache, confirm
    redirect within ~2.5s.
  - Verify a normal apt operation against a healthy mirror still works end to
    end with default flags.

## Migration & Compatibility

Upgrading without setting any flags changes behavior in three observable ways:

1. The cache-hit validation path now fails over to stale within ~5s instead of
   ~50s. This is the intended fix.
2. Cache-miss transfers are now bounded at 120s instead of 300s. Any healthy
   upstream completes well within both; only pathological transfers are affected,
   and those were already broken from the client side (ALB + Bazel timeouts).
3. `ResponseHeaderTimeout` drops from 30s to 20s. Same argument as (2).

No flag changes required for existing deployers. Operators with unusual upstreams
can restore the old behavior by setting:

- `MIRROR_CACHE_VALIDATION_TIMEOUT=0` (disables the deadline)
- `MIRROR_CACHE_CLIENT_TIMEOUT=300s`
- `MIRROR_CACHE_RESPONSE_HEADER_TIMEOUT=30s`

No on-disk state is affected. No changes to `httpCache` interface, `CacheKey`
shape, S3 path layout, fallback policy, or logging contract.

## Decisions

- **Validation deadline is decoupled from `FallbackPolicy`.** The deadline
  fires regardless of `OnConnectionError`. If an operator has set
  `--stale-on-connection-error=false`, they're saying "I want clients to know
  upstream is broken instead of getting stale data" — telling them in 5s is
  strictly better than telling them in 50s. The deadline bounds how long we
  wait to learn the upstream is unreachable; the fallback policy decides what
  to do with that knowledge. Keeping these orthogonal is the correct factoring.

## Open Questions

- **Negative-result memoization (alternative 5).** Should a recent validation
  failure short-circuit subsequent validations to the fallback path for, say,
  30s? It would smooth out a thundering-herd retry pattern when many clients hit
  the proxy during an upstream outage, but it adds in-process state and a TTL
  that interacts subtly with the singleflight key. Flagged but not designed
  here; revisit if outage-time CPU/connection load on the proxy itself becomes a
  concern.
