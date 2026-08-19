package freshness_test

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/thomasdesr/external-mirror-cache/internal/freshness"
)

const day = 24 * time.Hour

var storedAt = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

func h(pairs ...string) http.Header {
	hdr := make(http.Header)
	for i := 0; i < len(pairs); i += 2 {
		hdr.Add(pairs[i], pairs[i+1])
	}

	return hdr
}

// The fixture tables below use the surveyed hosts' exact header sets,
// verified against production capture, plus the edge cases the ACs name.

// TestEvaluateLifetimes covers lifetime precedence and age arithmetic
// (rfc9111-freshness.AC1.2–AC1.4).
func TestEvaluateLifetimes(t *testing.T) {
	runEvaluateCases(t, []evaluateCase{
		{
			// files.pythonhosted.org: the dominant traffic source, immutable content.
			name:       "pythonhosted fresh within cap",
			stored:     h("Cache-Control", "max-age=365000000, immutable, public", "Etag", `"abc"`),
			elapsed:    time.Hour,
			cap:        7 * day,
			wantFresh:  true,
			wantReason: freshness.ReasonFresh,
		},
		{
			// rfc9111-freshness.AC3.1: reason distinguishes cap from TTL.
			name:       "pythonhosted stale at cap boundary is cap-expired",
			stored:     h("Cache-Control", "max-age=365000000, immutable, public"),
			elapsed:    7 * day,
			cap:        7 * day,
			wantFresh:  false,
			wantReason: freshness.ReasonCapExpired,
		},
		{
			// rfc9111-freshness.AC1.9: a CDN-fronted immutable file can carry
			// weeks of CDN residency in its Age. The cap bounds resident time
			// (our time since storing), not Age — an entry stored an hour ago
			// is fresh no matter how long the CDN held it first.
			name:       "pythonhosted CDN Age beyond cap still fresh when recently stored",
			stored:     h("Cache-Control", "max-age=365000000, immutable, public", "Age", "1728000"),
			elapsed:    time.Hour,
			cap:        7 * day,
			wantFresh:  true,
			wantReason: freshness.ReasonFresh,
		},
		{
			// When both axes have expired, the upstream's own declaration
			// expiring is the primary fact; cap-expired is reserved for
			// entries only our cap makes stale.
			name:       "expired on both axes reports ttl expired",
			stored:     h("Cache-Control", "max-age=900"),
			elapsed:    8 * day,
			cap:        7 * day,
			wantFresh:  false,
			wantReason: freshness.ReasonTTLExpired,
		},
		{
			// rfc9111-freshness.AC1.2: cdn.azul.com via Fastly, s-maxage only,
			// delivered pre-aged. Age: 11866 shortens the 86400s window.
			name:       "azul s-maxage minus Age still fresh",
			stored:     h("Cache-Control", "s-maxage=86400", "Age", "11866"),
			elapsed:    (86400 - 11866 - 1) * time.Second,
			cap:        7 * day,
			wantFresh:  true,
			wantReason: freshness.ReasonFresh,
		},
		{
			// rfc9111-freshness.AC1.4: equality is stale (strict inequality).
			name:       "azul stale exactly at lifetime",
			stored:     h("Cache-Control", "s-maxage=86400", "Age", "11866"),
			elapsed:    (86400 - 11866) * time.Second,
			cap:        7 * day,
			wantFresh:  false,
			wantReason: freshness.ReasonTTLExpired,
		},
		{
			// rfc9111-freshness.AC1.3: Expires with Date, no max-age.
			name:       "debian Expires minus Date fresh",
			stored:     h("Date", storedAt.Format(http.TimeFormat), "Expires", storedAt.Add(time.Hour).Format(http.TimeFormat)),
			elapsed:    59 * time.Minute,
			cap:        7 * day,
			wantFresh:  true,
			wantReason: freshness.ReasonFresh,
		},
		{
			// rfc9111-freshness.AC1.3: max-age wins over Expires.
			name: "max-age wins over shorter Expires",
			stored: h(
				"Cache-Control", "max-age=7200",
				"Date", storedAt.Format(http.TimeFormat),
				"Expires", storedAt.Add(time.Hour).Format(http.TimeFormat),
			),
			elapsed:    90 * time.Minute,
			cap:        7 * day,
			wantFresh:  true,
			wantReason: freshness.ReasonFresh,
		},
		{
			// Expires without Date anchors to storedAt (RFC 9110 §6.6.1).
			name:       "Expires without Date anchors to storedAt",
			stored:     h("Expires", storedAt.Add(30*time.Minute).Format(http.TimeFormat)),
			elapsed:    29 * time.Minute,
			cap:        7 * day,
			wantFresh:  true,
			wantReason: freshness.ReasonFresh,
		},
		{
			// Skew-broken origin: Expires before Date. Negative lifetime,
			// never fresh, never an error.
			name:       "Expires before Date never fresh",
			stored:     h("Date", storedAt.Format(http.TimeFormat), "Expires", storedAt.Add(-time.Hour).Format(http.TimeFormat)),
			elapsed:    0,
			cap:        7 * day,
			wantFresh:  false,
			wantReason: freshness.ReasonTTLExpired,
		},
	})
}

// TestEvaluateAlwaysRevalidates covers entries that must revalidate on every
// request: no-cache in every form, validator-only hosts, and zero lifetimes
// (rfc9111-freshness.AC2.1–AC2.3).
func TestEvaluateAlwaysRevalidates(t *testing.T) {
	runEvaluateCases(t, []evaluateCase{
		{
			// rfc9111-freshness.AC2.1: bcr.bazel.build, one of the busiest hosts.
			name:       "bcr no-cache never fresh",
			stored:     h("Cache-Control", "public, no-cache"),
			elapsed:    0,
			cap:        7 * day,
			wantFresh:  false,
			wantReason: freshness.ReasonNoCache,
		},
		{
			// §5.2.2.4: the argumented form is deliberately treated the same
			// as bare no-cache — over-conservatism that fails open.
			name:       "argumented no-cache disqualifies despite max-age",
			stored:     h("Cache-Control", `no-cache="set-cookie", max-age=3600`),
			elapsed:    0,
			cap:        7 * day,
			wantFresh:  false,
			wantReason: freshness.ReasonNoCache,
		},
		{
			// no-cache inside a quoted string is data, not a directive.
			name:       "no-cache inside quoted string is not no-cache",
			stored:     h("Cache-Control", `private="no-cache, max-age=99", max-age=3600`),
			elapsed:    time.Minute,
			cap:        7 * day,
			wantFresh:  true,
			wantReason: freshness.ReasonFresh,
		},
		{
			// rfc9111-freshness.AC2.2: validator-only hosts get no heuristic
			// window, ever (apt.postgresql.org shape).
			name:       "validators only means no lifetime",
			stored:     h("Etag", `"abc"`, "Last-Modified", storedAt.Format(http.TimeFormat)),
			elapsed:    0,
			cap:        7 * day,
			wantFresh:  false,
			wantReason: freshness.ReasonNoLifetime,
		},
		{
			// rfc9111-freshness.AC2.3: PyPI's max-age=0 canary keeps working.
			name:       "max-age zero always revalidates",
			stored:     h("Cache-Control", "max-age=0"),
			elapsed:    0,
			cap:        7 * day,
			wantFresh:  false,
			wantReason: freshness.ReasonTTLExpired,
		},
		{
			// rfc9111-freshness.AC2.3: s-maxage takes shared-cache precedence.
			name:       "s-maxage zero wins over large max-age",
			stored:     h("Cache-Control", "s-maxage=0, max-age=365000000"),
			elapsed:    0,
			cap:        7 * day,
			wantFresh:  false,
			wantReason: freshness.ReasonTTLExpired,
		},
	})
}

// TestEvaluateVaryGuard covers the Vary guard (rfc9111-freshness.AC3.2,
// AC3.3 at the function level).
func TestEvaluateVaryGuard(t *testing.T) {
	runEvaluateCases(t, []evaluateCase{
		{
			// rfc9111-freshness.AC3.2: pypi.org/simple on a URL-only key.
			name:       "vary on URL-only key never fresh",
			stored:     h("Cache-Control", "max-age=600", "Vary", "Accept"),
			elapsed:    0,
			cap:        7 * day,
			keyHeaders: nil,
			wantFresh:  false,
			wantReason: freshness.ReasonVaryMismatch,
		},
		{
			// rfc9111-freshness.AC3.3: OCI Accept-variant keys tolerate
			// Vary: Accept.
			name:       "vary Accept satisfied by Accept-variant key",
			stored:     h("Cache-Control", "max-age=600", "Vary", "accept"),
			elapsed:    time.Minute,
			cap:        7 * day,
			keyHeaders: []string{"Accept"},
			wantFresh:  true,
			wantReason: freshness.ReasonFresh,
		},
		{
			// rfc9111-freshness.AC3.2: Vary: * never matches (RFC 9111 §4.1).
			name:       "vary star never fresh",
			stored:     h("Cache-Control", "max-age=600", "Vary", "*"),
			elapsed:    0,
			cap:        7 * day,
			keyHeaders: []string{"Accept"},
			wantFresh:  false,
			wantReason: freshness.ReasonVaryMismatch,
		},
		{
			name:       "vary naming unencoded header never fresh",
			stored:     h("Cache-Control", "max-age=600", "Vary", "Accept, Accept-Encoding"),
			elapsed:    0,
			cap:        7 * day,
			keyHeaders: []string{"Accept"},
			wantFresh:  false,
			wantReason: freshness.ReasonVaryMismatch,
		},
	})
}

// TestEvaluateFailOpen covers rfc9111-freshness.AC5.1: every malformed
// input degrades to revalidation, never to an error.
func TestEvaluateFailOpen(t *testing.T) {
	runEvaluateCases(t, []evaluateCase{
		{
			// rfc9111-freshness.AC5.1: garbage Age disqualifies rather than
			// counting as zero — zero would make the entry look younger.
			name:       "unparseable Age disqualifies",
			stored:     h("Cache-Control", "max-age=3600", "Age", "banana"),
			elapsed:    0,
			cap:        7 * day,
			wantFresh:  false,
			wantReason: freshness.ReasonInvalidAge,
		},
		{
			name:       "negative Age disqualifies",
			stored:     h("Cache-Control", "max-age=3600", "Age", "-5"),
			elapsed:    0,
			cap:        7 * day,
			wantFresh:  false,
			wantReason: freshness.ReasonInvalidAge,
		},
		{
			// An Age near the Duration ceiling must not wrap the age sum
			// negative — a wrapped sum reads fresher than any lifetime,
			// turning one garbage header into a permanent cache pin.
			name:       "near-overflow Age disqualifies",
			stored:     h("Cache-Control", "max-age=100", "Age", "9223372036"),
			elapsed:    2 * time.Second,
			cap:        7 * day,
			wantFresh:  false,
			wantReason: freshness.ReasonInvalidAge,
		},
		{
			// rfc9111-freshness.AC5.1: missing stored-at degrades to
			// revalidation, never an error.
			name:       "zero storedAt disqualifies",
			stored:     h("Cache-Control", "max-age=3600"),
			storedAt:   time.Time{},
			elapsed:    0,
			cap:        7 * day,
			wantFresh:  false,
			wantReason: freshness.ReasonNoStoredAt,
		},
		{
			name:       "garbage max-age means no lifetime",
			stored:     h("Cache-Control", "max-age=banana"),
			elapsed:    0,
			cap:        7 * day,
			wantFresh:  false,
			wantReason: freshness.ReasonNoLifetime,
		},
		{
			name:       "overflowing max-age means no lifetime",
			stored:     h("Cache-Control", "max-age=99999999999999999999"),
			elapsed:    0,
			cap:        7 * day,
			wantFresh:  false,
			wantReason: freshness.ReasonNoLifetime,
		},
		{
			// Conflicting duplicates are ambiguity; ambiguity fails open.
			name:       "duplicate max-age means no lifetime",
			stored:     h("Cache-Control", "max-age=100, max-age=200"),
			elapsed:    0,
			cap:        7 * day,
			wantFresh:  false,
			wantReason: freshness.ReasonNoLifetime,
		},
		{
			// Ambiguity in the strongest directive must disqualify the
			// lifetime outright, never surrender to a weaker directive:
			// every s-maxage copy here says always-revalidate.
			name: "duplicate s-maxage does not fall through to max-age",
			//nolint:dupword // the duplicated directive is the case under test
			stored:     h("Cache-Control", "s-maxage=0, s-maxage=0, max-age=365000000"),
			elapsed:    0,
			cap:        7 * day,
			wantFresh:  false,
			wantReason: freshness.ReasonNoLifetime,
		},
		{
			name: "garbage max-age does not fall through to Expires",
			stored: h(
				"Cache-Control", "max-age=banana",
				"Date", storedAt.Format(http.TimeFormat),
				"Expires", storedAt.Add(time.Hour).Format(http.TimeFormat),
			),
			elapsed:    0,
			cap:        7 * day,
			wantFresh:  false,
			wantReason: freshness.ReasonNoLifetime,
		},
		{
			name:       "unparseable Expires means no lifetime",
			stored:     h("Expires", "0"),
			elapsed:    0,
			cap:        7 * day,
			wantFresh:  false,
			wantReason: freshness.ReasonNoLifetime,
		},
	})
}

type evaluateCase struct {
	name       string
	stored     http.Header
	storedAt   time.Time // zero means the storedAt package default
	elapsed    time.Duration
	cap        time.Duration
	keyHeaders []string
	wantFresh  bool
	wantReason freshness.Reason
}

func runEvaluateCases(t *testing.T, cases []evaluateCase) {
	t.Helper()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			at := tc.storedAt
			if at.IsZero() && tc.wantReason != freshness.ReasonNoStoredAt {
				at = storedAt
			}

			d := freshness.Evaluate(tc.stored, at, at.Add(tc.elapsed), tc.cap, tc.keyHeaders)
			if d.Fresh != tc.wantFresh || d.Reason != tc.wantReason {
				t.Fatalf("Evaluate() = (fresh=%v, reason=%q), want (fresh=%v, reason=%q)",
					d.Fresh, d.Reason, tc.wantFresh, tc.wantReason)
			}
		})
	}
}

// TestEvaluateObservabilityFields pins the fields the fresh log line carries.
func TestEvaluateObservabilityFields(t *testing.T) {
	stored := h("Cache-Control", "max-age=365000000", "Age", "100")

	d := freshness.Evaluate(stored, storedAt, storedAt.Add(time.Hour), 7*day, nil)
	if !d.Fresh {
		t.Fatalf("Evaluate() not fresh: %+v", d)
	}

	if want := time.Hour + 100*time.Second; d.Age != want {
		t.Errorf("Age = %v, want %v", d.Age, want)
	}

	if want := 365000000 * time.Second; d.DeclaredLifetime != want {
		t.Errorf("DeclaredLifetime = %v, want %v", d.DeclaredLifetime, want)
	}

	if want := time.Hour; d.Resident != want {
		t.Errorf("Resident = %v, want %v", d.Resident, want)
	}
}

// Property: an entry fresh at time t is fresh at every earlier time back to
// storedAt (age grows monotonically; nothing else varies).
func TestFreshnessMonotonic(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		maxAge := rapid.Int64Range(0, 1<<32).Draw(t, "maxAge")
		capSecs := rapid.Int64Range(1, 30*86400).Draw(t, "capSecs")
		storedAge := rapid.Int64Range(0, 1<<20).Draw(t, "storedAge")
		e2 := rapid.Int64Range(0, 40*86400).Draw(t, "e2")
		e1 := rapid.Int64Range(0, e2).Draw(t, "e1")

		stored := h(
			"Cache-Control", fmt.Sprintf("max-age=%d", maxAge),
			"Age", strconv.FormatInt(storedAge, 10),
		)
		lifetimeCap := time.Duration(capSecs) * time.Second

		at := func(elapsed int64) freshness.Decision {
			return freshness.Evaluate(stored, storedAt, storedAt.Add(time.Duration(elapsed)*time.Second), lifetimeCap, nil)
		}

		if at(e2).Fresh && !at(e1).Fresh {
			t.Fatalf("fresh at +%ds but stale at earlier +%ds", e2, e1)
		}
	})
}

// Property: never fresh once resident time (now − storedAt) reaches the
// cap, regardless of the declared lifetime or stored Age (cap dominance,
// rfc9111-freshness.AC3.1's function half).
func TestCapDominates(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		maxAge := rapid.Int64Range(0, 1<<40).Draw(t, "maxAge")
		capSecs := rapid.Int64Range(1, 30*86400).Draw(t, "capSecs")
		past := rapid.Int64Range(0, 30*86400).Draw(t, "past")
		storedAge := rapid.Int64Range(0, 1<<20).Draw(t, "storedAge")

		stored := h(
			"Cache-Control", fmt.Sprintf("max-age=%d", maxAge),
			"Age", strconv.FormatInt(storedAge, 10),
		)
		elapsed := time.Duration(capSecs+past) * time.Second

		d := freshness.Evaluate(stored, storedAt, storedAt.Add(elapsed), time.Duration(capSecs)*time.Second, nil)
		if d.Fresh {
			t.Fatalf("fresh with elapsed %v >= cap %ds (max-age=%d, Age=%d)", elapsed, capSecs, maxAge, storedAge)
		}
	})
}

// Property: a stored Age of k seconds shifts the declared-lifetime axis by
// exactly k — with a never-binding cap, evaluating with Age: k at elapsed e
// reaches the same verdict as evaluating without Age at elapsed e+k — and
// leaves the resident axis untouched: no Age value can produce a cap expiry
// while resident time is under the cap (rfc9111-freshness.AC1.5, AC1.9).
func TestAgeShiftsDeclaredAxisExactly(t *testing.T) {
	// k spans the full range parseDeltaSeconds accepts, so the property
	// also proves the accepted range cannot overflow the age sum.
	maxValidDelta := int64(math.MaxInt64) / int64(time.Second) / 2

	rapid.Check(t, func(t *rapid.T) {
		maxAge := rapid.Int64Range(0, 1<<32).Draw(t, "maxAge")
		k := rapid.Int64Range(0, maxValidDelta).Draw(t, "k")
		e := rapid.Int64Range(0, 40*86400).Draw(t, "e")

		cc := fmt.Sprintf("max-age=%d", maxAge)
		aged := h("Cache-Control", cc, "Age", strconv.FormatInt(k, 10))
		unbounded := time.Duration(math.MaxInt64)

		withAge := freshness.Evaluate(aged, storedAt, storedAt.Add(time.Duration(e)*time.Second), unbounded, nil)
		shifted := freshness.Evaluate(
			h("Cache-Control", cc),
			storedAt, storedAt.Add(time.Duration(e+k)*time.Second), unbounded, nil,
		)

		if withAge.Fresh != shifted.Fresh || withAge.Reason != shifted.Reason || withAge.Age != shifted.Age {
			t.Fatalf("Age:%d at +%ds = %+v, no Age at +%ds = %+v", k, e, withAge, e+k, shifted)
		}

		if withAge.Age < 0 {
			t.Fatalf("Age:%d at +%ds computed negative age %v (overflow)", k, e, withAge.Age)
		}

		capJustAboveElapsed := time.Duration(e)*time.Second + time.Second

		d := freshness.Evaluate(aged, storedAt, storedAt.Add(time.Duration(e)*time.Second), capJustAboveElapsed, nil)
		if d.Reason == freshness.ReasonCapExpired {
			t.Fatalf("Age:%d tripped the cap with resident %ds under cap %v", k, e, capJustAboveElapsed)
		}
	})
}
