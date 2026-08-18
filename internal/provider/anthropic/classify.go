package anthropic

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nicko170/aiproxy/internal/provider"
)

const bucketPrefix = "anthropic-ratelimit-unified-"

// windowName matches the name of a genuine quota window: a duration — one or
// more digits then a unit — optionally carrying a model scope ("5h", "7d",
// "7d_oi").
//
// The upstream publishes more than windows under the shared
// "anthropic-ratelimit-unified-" prefix. A live response also carries
// "overage-status", "overage-disabled-reason", "representative-claim" and
// "fallback-percentage", none of which describe an allowance. Admitting them
// as buckets caused a production outage: "overage-status: rejected" reports
// that overage *billing* is disabled for the organisation — the default for
// most orgs, restated by the sibling "overage-disabled-reason" — but it
// reached account selection as a rejected, unscoped bucket and made every
// account permanently ineligible after its first successful response.
//
// Windows are therefore recognised by shape, not by blacklisting the metadata
// names seen today, so a metadata header added upstream tomorrow cannot
// resurrect this. The leading digit is the discriminator; the unit is allowed
// more than one letter so a future "30min" or "1mo" window still parses.
var windowName = regexp.MustCompile(`^[0-9]+[a-z]+(_[a-z0-9]+)*$`)

// isWindowName reports whether a parsed bucket name is a quota window rather
// than metadata that merely shares the header prefix.
func isWindowName(name string) bool { return windowName.MatchString(name) }

// Classify maps an upstream response to an Outcome. It is pure: no network, no
// clock, no state, so every rate-limit shape is a table test.
//
// The ordering matters. A quota rejection is evidence that waiting cannot help,
// so it outranks a Retry-After hint that might otherwise suggest a short pause.
// And a 429 carrying neither a hint nor bucket headers must classify as
// ThrottledNoHint rather than acquiring a default duration: a fabricated wait,
// absorbed inline across several accounts, is what turns a sub-second upstream
// rejection into a multi-minute silent hold.
func Classify(r *http.Response) provider.Outcome {
	out := provider.Outcome{Buckets: parseBuckets(r.Header)}

	switch {
	case r.StatusCode == http.StatusTooManyRequests:
		if name, ok := rejectedBucket(out.Buckets); ok {
			out.Kind = provider.OutcomeQuotaRejected
			if isModelScoped(name) {
				out.ScopedModel = name
			}
			return out
		}
		if d, ok := retryAfter(r.Header); ok {
			out.Kind = provider.OutcomeThrottledWithHint
			out.RetryAfter = d
			return out
		}
		out.Kind = provider.OutcomeThrottledNoHint
		return out

	case r.StatusCode == http.StatusUnauthorized:
		out.Kind = provider.OutcomeCredentialStale
	case r.StatusCode == http.StatusForbidden:
		out.Kind = provider.OutcomeCredentialRefused
	case r.StatusCode >= 500:
		out.Kind = provider.OutcomeServerError
	case r.StatusCode >= 400:
		out.Kind = provider.OutcomeClientError
	default:
		out.Kind = provider.OutcomeOK
	}
	return out
}

// retryAfter reads a delta-seconds Retry-After. A missing, unparseable, or
// negative value reports ok=false so the caller falls through to the no-hint
// path instead of inventing a duration. Zero is a real hint of "immediately".
func retryAfter(h http.Header) (time.Duration, bool) {
	raw := strings.TrimSpace(h.Get("Retry-After"))
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, false
	}
	return time.Duration(n) * time.Second, true
}

func rejectedBucket(buckets []provider.QuotaBucket) (string, bool) {
	// A general rejection outranks a model-scoped one: it holds the whole
	// account, so report it even if a scoped bucket is also rejected.
	scoped := ""
	for _, b := range buckets {
		if b.Status != "rejected" {
			continue
		}
		if !isModelScoped(b.Name) {
			return b.Name, true
		}
		scoped = b.Name
	}
	if scoped != "" {
		return scoped, true
	}
	return "", false
}

// modelScope matches a window name carrying a KNOWN model scope, e.g. "7d_oi"
// for the per-model weekly cap versus plain "7d".
//
// Recognised by shape plus a known suffix, not by "contains an underscore",
// because the two ways of being wrong here are not symmetric:
//
//   - Calling a scoped bucket general holds the whole account when only one
//     model family is spent. That costs availability, and it is recoverable.
//   - Calling a GENERAL bucket scoped sets ScopedModel, which makes the attempt
//     loop skip MarkRateLimited entirely — a spent account stays selectable and
//     keeps being sent to. That fails OPEN, which is exactly the direction the
//     overage fix and modelBucketName's own note exist to rule out.
//
// A future unscoped window is shaped identically to a scoped one ("24h_soft"
// versus "7d_oi"), so shape alone cannot separate them and anything
// unrecognised must land on the safe side. The cost is that a genuinely new
// model scope binds the whole account until this list learns about it — the
// conservative failure, taken deliberately.
var modelScope = regexp.MustCompile(`^[0-9]+[a-z]+_(oi|opus|sonnet|haiku|fable)$`)

func isModelScoped(name string) bool { return modelScope.MatchString(name) }

// parseBuckets collects anthropic-ratelimit-unified-<window>-<field> headers
// into one QuotaBucket per <window>.
//
// Only genuine quota windows become buckets, and only when at least one field
// carried a usable value: everything else in this header namespace is metadata
// that must never be presented to account selection as an allowance.
//
// The utilization field here is a 0..1 FRACTION, not a percentage — see
// scale.go for the live evidence. This must not be divided by 100: that is
// bucketValue's conversion (usage.go), for the JSON endpoint's percentage,
// and applying it here silently made every observed utilization 100x too
// small.
func parseBuckets(h http.Header) []provider.QuotaBucket {
	byName := map[string]*provider.QuotaBucket{}
	// usable records that a window contributed at least one field we could
	// read. Without it a stray "anthropic-ratelimit-unified-5h-anything"
	// header would mint a phantom window with no status, no utilization and
	// no reset, which selection would then have to reason about.
	usable := map[string]bool{}
	// suspect records a window whose utilization header was out of range for
	// the 0..1 fraction scale (normalizeUtilization, scale.go). Applied after
	// the loop below, not inline, because http.Header iteration order is
	// unspecified — a "status: allowed" header processed after this one must
	// not silently undo the fail-closed marking.
	suspect := map[string]bool{}
	order := []string{}

	for key := range h {
		lower := strings.ToLower(key)
		if !strings.HasPrefix(lower, bucketPrefix) {
			continue
		}
		rest := lower[len(bucketPrefix):]
		idx := strings.LastIndex(rest, "-")
		if idx <= 0 {
			// No window name, just a field: "anthropic-ratelimit-unified-status"
			// and "-reset" are the overall unified values for the account as a
			// whole. Dropping them is deliberate. Eligibility is decided per
			// window, and a nameless bucket carries no window to decide about —
			// it would bind every model while duplicating whichever real window
			// the upstream currently considers representative.
			continue
		}
		name, field := rest[:idx], rest[idx+1:]
		if !isWindowName(name) {
			continue
		}

		value := strings.TrimSpace(h.Get(key))
		b, ok := byName[name]
		if !ok {
			b = &provider.QuotaBucket{Name: name}
			byName[name] = b
			order = append(order, name)
		}
		switch field {
		case "status":
			if value != "" {
				b.Status = value
				usable[name] = true
			}
		case "utilization":
			if f, err := strconv.ParseFloat(value, 64); err == nil {
				if frac, ok := normalizeUtilization(f, fractionScale); ok {
					b.Utilization = frac
				} else {
					// Out of range for a 0..1 header: fail closed rather
					// than store a fabricated fraction — see scale.go.
					suspect[name] = true
				}
				usable[name] = true
			}
		case "reset":
			if ms := toUnixMillis(value); ms != 0 {
				b.ResetsAt = ms
				usable[name] = true
			}
		}
	}

	out := make([]provider.QuotaBucket, 0, len(order))
	for _, name := range order {
		if !usable[name] {
			continue
		}
		b := *byName[name]
		if suspect[name] {
			// An out-of-range utilization means we no longer trust this
			// source's scale for this window. Mark it rejected — the same
			// verdict eligibleLocked already gives an upstream "rejected" —
			// rather than let a fabricated 0..1 number decide selection.
			b.Status = "rejected"
		}
		out = append(out, b)
	}
	return out
}

// toUnixMillis accepts unix seconds, unix milliseconds, or RFC3339 and returns
// unix milliseconds. Returns 0 when the value is unusable.
func toUnixMillis(v string) int64 {
	if v == "" {
		return 0
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n < 1e12 { // seconds, not milliseconds
			return n * 1000
		}
		return n
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UnixMilli()
	}
	return 0
}
