// Package freshness implements the RFC 9111 freshness gate described in
// docs/design-plans/2026-08-04-rfc9111-freshness.md: a pure decision function
// over (stored headers, stored-at time, now, cap) that reports whether a
// cached entry may be served without revalidation, and the §4.3.4 header
// merge applied when a revalidation confirms the entry unchanged.
//
// Every ambiguity fails open to revalidation: the gate can only remove
// upstream requests that were provably unnecessary, never create a new error
// path.
package freshness

import (
	"math"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Decision is the gate's verdict plus the fields observability needs.
type Decision struct {
	Fresh  bool
	Reason Reason

	// Age is the entry's computed current age: (now − storedAt) + stored Age.
	Age time.Duration
	// DeclaredLifetime is the upstream-declared freshness lifetime
	// (s-maxage, else max-age, else Expires−Date). Zero when Reason is
	// ReasonNoLifetime.
	DeclaredLifetime time.Duration
	// EffectiveLifetime is min(DeclaredLifetime, cap).
	EffectiveLifetime time.Duration
}

// Reason explains a Decision, in the vocabulary the structured logs use.
type Reason string

// The gate's reasons: ReasonFresh accompanies the one fresh outcome; the
// rest say which eligibility condition failed, with TTL- and cap-expiry
// deliberately distinguished for the 3am operator.
const (
	ReasonFresh        Reason = "within freshness lifetime"
	ReasonNoCache      Reason = "no-cache"
	ReasonVaryMismatch Reason = "vary mismatch"
	ReasonNoLifetime   Reason = "no declared lifetime"
	ReasonInvalidAge   Reason = "invalid age"
	ReasonNoStoredAt   Reason = "missing stored-at"
	ReasonTTLExpired   Reason = "ttl expired"
	ReasonCapExpired   Reason = "cap expired"
)

// Evaluate decides whether a cached entry is fresh at now. storedAt is when
// the entry was written (S3 LastModified); lifetimeCap globally bounds any
// declared lifetime; keyHeaders are the header names the entry's cache-key
// variant encodes (the Vary guard tolerates only those).
func Evaluate(stored http.Header, storedAt, now time.Time, lifetimeCap time.Duration, keyHeaders []string) Decision {
	if storedAt.IsZero() {
		return Decision{Reason: ReasonNoStoredAt}
	}

	directives := parseCacheControl(stored.Values("Cache-Control"))
	if directives.noCache {
		// §5.2.2.4: may store, must revalidate before every reuse. The
		// argumented form no-cache="field" is deliberately treated the same
		// as bare no-cache — over-conservatism that fails open.
		return Decision{Reason: ReasonNoCache}
	}

	if !varySatisfied(stored.Values("Vary"), keyHeaders) {
		return Decision{Reason: ReasonVaryMismatch}
	}

	lifetime, ok := declaredLifetime(directives, stored, storedAt)
	if !ok {
		return Decision{Reason: ReasonNoLifetime}
	}

	storedAge, ok := parseAge(stored.Values("Age"))
	if !ok {
		// Counting garbage as zero would make the entry look younger and
		// reward malformed input with a fresh serve; disqualify instead.
		return Decision{Reason: ReasonInvalidAge}
	}

	d := Decision{
		Age:               now.Sub(storedAt) + storedAge,
		DeclaredLifetime:  lifetime,
		EffectiveLifetime: min(lifetime, lifetimeCap),
	}

	switch {
	case d.Age < d.EffectiveLifetime: // §4.2: equality is stale
		d.Fresh = true
		d.Reason = ReasonFresh
	case lifetime <= lifetimeCap:
		d.Reason = ReasonTTLExpired
	default:
		d.Reason = ReasonCapExpired
	}

	return d
}

// ccDirectives is the subset of Cache-Control the gate reads. A duration is
// present only when the directive appeared exactly once with a valid
// delta-seconds value; duplicates and garbage read as absent.
type ccDirectives struct {
	noCache bool
	sMaxAge deltaSeconds
	maxAge  deltaSeconds
}

type deltaSeconds struct {
	d    time.Duration
	ok   bool
	seen bool // directive appeared at all, even duplicated or garbage
}

// parseCacheControl splits the comma-separated directive list across all
// Cache-Control values — quoted-string aware, case-insensitive token match,
// not a substring search.
func parseCacheControl(values []string) ccDirectives {
	occurrences := make(map[string][]string)

	for _, value := range values {
		for _, directive := range splitQuoted(value) {
			name, arg, _ := strings.Cut(directive, "=")

			name = strings.ToLower(strings.TrimSpace(name))
			if name == "" {
				continue
			}

			occurrences[name] = append(occurrences[name], strings.TrimSpace(arg))
		}
	}

	parseOnce := func(name string) deltaSeconds {
		args := occurrences[name]
		if len(args) == 0 {
			return deltaSeconds{}
		}

		if len(args) > 1 {
			return deltaSeconds{seen: true}
		}

		parsed := parseDeltaSeconds(args[0])
		parsed.seen = true

		return parsed
	}

	return ccDirectives{
		noCache: len(occurrences["no-cache"]) > 0,
		sMaxAge: parseOnce("s-maxage"),
		maxAge:  parseOnce("max-age"),
	}
}

// splitQuoted splits s on commas that are outside double-quoted strings,
// so a directive argument like private="set-cookie, x" stays one piece.
func splitQuoted(s string) []string {
	var parts []string

	start, inQuotes := 0, false

	for i := range len(s) {
		switch s[i] {
		case '"':
			inQuotes = !inQuotes
		case ',':
			if !inQuotes {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}

	return append(parts, s[start:])
}

// varySatisfied reports whether every header name in the stored Vary values
// is one the cache-key variant encodes. Vary: * never matches (RFC 9111
// §4.1); an absent or empty Vary always does.
func varySatisfied(varyValues, keyHeaders []string) bool {
	for _, value := range varyValues {
		for name := range strings.SplitSeq(value, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}

			if name == "*" || !slices.ContainsFunc(keyHeaders, func(k string) bool { return strings.EqualFold(k, name) }) {
				return false
			}
		}
	}

	return true
}

// declaredLifetime applies the shared-cache precedence of RFC 9111 §5.2.2.10
// and §4.2.1: s-maxage, else max-age, else Expires−Date. When Date is absent
// or unparseable, storedAt substitutes — RFC 9110 §6.6.1 lets a recipient
// assume Date is reception time, and storedAt is our reception time. The
// returned lifetime may be negative (Expires in the past); never fresh, by
// the strict age comparison.
func declaredLifetime(directives ccDirectives, stored http.Header, storedAt time.Time) (time.Duration, bool) {
	// A directive that is present but duplicated or unparseable disqualifies
	// the lifetime outright rather than falling through: surrendering to a
	// weaker source would let ambiguity grant a window the stronger
	// directive denied (s-maxage=0 twice must not yield max-age's year).
	if directives.sMaxAge.seen {
		return directives.sMaxAge.d, directives.sMaxAge.ok
	}

	if directives.maxAge.seen {
		return directives.maxAge.d, directives.maxAge.ok
	}

	expiresValues := stored.Values("Expires")
	if len(expiresValues) != 1 {
		return 0, false
	}

	expires, err := http.ParseTime(expiresValues[0])
	if err != nil {
		return 0, false
	}

	anchor := storedAt

	if dateValues := stored.Values("Date"); len(dateValues) == 1 {
		if date, err := http.ParseTime(dateValues[0]); err == nil {
			anchor = date
		}
	}

	return expires.Sub(anchor), true
}

// parseAge reads the stored Age header: absent means zero; a duplicated,
// non-numeric, negative, or overflowing value is invalid.
func parseAge(values []string) (time.Duration, bool) {
	switch len(values) {
	case 0:
		return 0, true
	case 1:
		age := parseDeltaSeconds(values[0])

		return age.d, age.ok
	default:
		return 0, false
	}
}

// parseDeltaSeconds parses the RFC 9110 delta-seconds grammar (1*DIGIT).
// Anything else — signs, quotes, non-digits, values that overflow int64
// seconds or time.Duration — is invalid.
func parseDeltaSeconds(s string) deltaSeconds {
	if s == "" || len(s) > 18 { // 18 digits caps below int64 overflow; real values are far smaller
		return deltaSeconds{}
	}

	var secs int64

	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return deltaSeconds{}
		}

		secs = secs*10 + int64(s[i]-'0')
	}

	// Half the Duration range, not all of it: the age arithmetic sums a
	// stored Age with elapsed time, and a value near the full range wraps
	// that sum negative — reading fresher than any lifetime, forever.
	// Elapsed is now minus an S3 timestamp (decades at most, never 146
	// years), so a half-range bound makes the sum unconditionally safe.
	if secs > math.MaxInt64/int64(time.Second)/2 {
		return deltaSeconds{}
	}

	return deltaSeconds{d: time.Duration(secs) * time.Second, ok: true}
}
