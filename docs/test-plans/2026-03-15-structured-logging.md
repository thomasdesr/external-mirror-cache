# Structured Logging — Human Test Plan

## Prerequisites

- Local build: `go build -o mirror-cache .` succeeds
- All automated tests pass: `go test -race -count=1 ./...` shows 77 tests passing across 5 packages
- AWS credentials configured with access to an S3 bucket
- The `mirror-cache` binary available in the working directory

## Phase 1: Log Format Selection (AC4.2)

| Step | Action | Expected |
|------|--------|----------|
| 1 | Open a terminal emulator and run `./mirror-cache --bucket test-bucket --log-level info 2>&1 \| head -5` | Output uses slog text format: lines like `time=2026-03-15T... level=INFO msg="starting mirror-cache" bucket=test-bucket prefix=cache listen=:8443 log_level=INFO`. No JSON braces. |
| 2 | Run `./mirror-cache --bucket test-bucket --log-level info 2>/tmp/mirror-cache-log.txt &` then `head -5 /tmp/mirror-cache-log.txt` | Output is JSON: each line starts with `{` and contains `"level":"INFO"`, `"msg":"starting mirror-cache"`, `"time":"..."` fields. This confirms non-TTY (file) produces JSON. |
| 3 | Kill the background process from step 2. | Process exits cleanly. |

## Phase 2: Log Level Control (AC3.1, AC3.2, AC3.3)

| Step | Action | Expected |
|------|--------|----------|
| 1 | Run `./mirror-cache --bucket test-bucket --log-level debug 2>&1 \| head -10` | Output includes DEBUG-level messages (e.g., lines with `level=DEBUG`). The startup message `starting mirror-cache` at INFO level is also visible. |
| 2 | Run `MIRROR_CACHE_LOG_LEVEL=warn ./mirror-cache --bucket test-bucket 2>&1 \| head -5` | No INFO-level startup message visible. Only WARN or higher messages appear (or no output if nothing triggers a warning during startup). The binary starts without error. |
| 3 | Run `./mirror-cache --bucket test-bucket --log-level banana 2>&1` | Binary exits immediately with error message containing `invalid log level "banana"`. Non-zero exit code. |
| 4 | Run `./mirror-cache --bucket test-bucket --log-level "" 2>&1` | Binary exits with error about invalid/empty log level. Non-zero exit code. |

## Phase 3: Request ID in Responses (AC2.3)

| Step | Action | Expected |
|------|--------|----------|
| 1 | Start the proxy: `./mirror-cache --bucket <your-bucket> --log-level debug 2>proxy.log &` | Process starts, listening on `:8443`. |
| 2 | Send a request: `curl -v http://localhost:8443/releases.hashicorp.com/terraform/1.10.0/terraform_1.10.0_linux_arm64.zip 2>&1 \| grep X-Request-ID` | Response includes `X-Request-ID:` header with a 16-character hex string (e.g., `X-Request-ID: a1b2c3d4e5f67890`). |
| 3 | Send a second request to the same URL and compare `X-Request-ID`. | The second request has a different `X-Request-ID` value than the first. |

## Phase 4: Structured Log Attributes (AC5.1, AC5.3)

| Step | Action | Expected |
|------|--------|----------|
| 1 | With the proxy still running from Phase 3, examine `proxy.log`: `cat proxy.log` | Log output is JSON (one JSON object per line, since stderr goes to a file). |
| 2 | Check for request lifecycle: `jq -r '.msg' proxy.log \| sort \| uniq -c` | Messages include "request started", "request completed", "fetching from upstream", and "cached upstream response". |
| 3 | Check target attribute: `jq 'select(.msg == "fetching from upstream")' proxy.log` | Each "fetching from upstream" JSON object contains a `"target"` field with the upstream URL. |
| 4 | Check 304 handling: send the same request again, then `jq 'select(.msg == "upstream returned 304, using cached content")' proxy.log` | The 304 log line contains a `"target"` field with the upstream URL. |
| 5 | Check S3 attributes: `jq 'select(.msg == "cache miss" or .msg == "uploading to cache" or .msg == "presigned URL generated")' proxy.log` | Each S3 operation log line contains both `"bucket"` (matching your bucket name) and `"key"` (matching the S3 path prefix + host + path). This validates AC5.3 in a live environment. |

## Phase 5: Fallback Logging (AC5.1 supplement)

| Step | Action | Expected |
|------|--------|----------|
| 1 | Cache a URL, then make upstream unreachable (e.g., use `--egress-proxy` pointing to a non-existent proxy). Send request for the cached URL. | Response is a 303 redirect to S3 (stale fallback). |
| 2 | Check logs: `jq 'select(.msg == "upstream error, serving stale")' proxy.log` | Log line contains `"target"` and `"error"` attributes. |

## End-to-End: Full Request Lifecycle

Validate that a single request produces a complete, traceable log trail from start to finish.

1. Start fresh proxy: `./mirror-cache --bucket <your-bucket> --log-level debug 2>e2e.log &`
2. Send a cache-miss request: `curl -s -o /dev/null -w '%{http_code}' -L http://localhost:8443/releases.hashicorp.com/terraform/1.10.0/terraform_1.10.0_SHA256SUMS`
3. Expected response: HTTP 200 (after following 303 redirect to S3 presigned URL).
4. Extract the request_id: `REQUEST_ID=$(jq -r 'select(.msg == "request started") | .request_id' e2e.log | head -1)`
5. Filter all logs for that request: `jq --arg rid "$REQUEST_ID" 'select(.request_id == $rid)' e2e.log`
6. Expected log sequence for that request_id:
   - `"request started"` with `method`, `path`, `request_id`
   - `"cache miss"` (Debug) with `bucket`, `key`
   - `"fetching from upstream"` (Debug) with `target`, `has_cached`
   - `"uploading to cache"` (Debug) with `bucket`, `key`
   - `"upload complete"` (Debug) with `bucket`, `key`
   - `"presigned URL generated"` (Debug) with `bucket`, `key`
   - `"cached upstream response"` (Debug) with `target`
   - `"request completed"` (Info) with `status` (303), `duration`, `request_id`
7. All log lines for this request_id must share the same `request_id` value.

## End-to-End: Singleflight Request ID Semantics

1. Start fresh proxy: `./mirror-cache --bucket <your-bucket> --log-level debug 2>sf.log &`
2. Launch two concurrent requests to the same uncached URL:
   ```
   curl -s -o /dev/null http://localhost:8443/releases.hashicorp.com/terraform/1.10.0/terraform_1.10.0_linux_arm64.zip &
   curl -s -o /dev/null http://localhost:8443/releases.hashicorp.com/terraform/1.10.0/terraform_1.10.0_linux_arm64.zip &
   wait
   ```
3. Count "request started" messages: `jq 'select(.msg == "request started")' sf.log | jq -r '.request_id' | sort -u | wc -l`
4. Expected: 2 distinct request_ids in "request started" messages.
5. Count "fetching from upstream" messages: `jq 'select(.msg == "fetching from upstream")' sf.log | wc -l`
6. Expected: Exactly 1 (singleflight deduplication means only leader fetches).

## Traceability

| Acceptance Criterion | Automated Test | Manual Step |
|----------------------|----------------|-------------|
| AC1.1 | grep verification + integration regression | -- |
| AC1.2 | grep verification | -- |
| AC2.1 | `TestNewRequestIDProperty` | -- |
| AC2.2 | `TestMiddlewareAllLogsHaveRequestID` | -- |
| AC2.3 | `TestMiddlewareSetXRequestIDHeader` | Phase 3, Step 2 |
| AC2.4 | `TestFromContextWithoutLogger`, `TestFromContextWithLogger` | -- |
| AC2.5 | `TestIntegration_SingleflightLeaderFollowerRequestIDs` | E2E: Singleflight |
| AC3.1 | `TestLogLevelFiltering("debug level shows debug")` | Phase 2, Step 1 |
| AC3.2 | `TestLogLevelFiltering("warn level hides info")` | Phase 2, Step 2 |
| AC3.3 | `TestLogLevelParsing("invalid level")` | Phase 2, Step 3 |
| AC4.1 | `TestIsTTYDetection`, `TestJSONHandlerOutput` | Phase 1, Step 2 |
| AC4.2 | -- (human only) | Phase 1, Step 1 |
| AC5.1 | `TestIntegration_TargetAttributeInLogs` | Phase 4, Steps 3-4 |
| AC5.2 | Skipped (file does not exist) | -- |
| AC5.3 | Accepted gap (fakeCache bypasses s3HTTPCache) | Phase 4, Step 5 |
