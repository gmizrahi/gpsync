// Package retryx classifies upload errors and computes backoff delays.
//
// Distinguishes: permanent failures (bad file — never retry), daily-quota
// exhaustion (pause the whole queue until the Pacific-time reset), and the
// undocumented per-minute/concurrent-write throttle (short backoff + shrink
// concurrency) from generic transient errors (5xx/network — plain backoff).
package retryx

import (
	"math/rand"
	"strconv"
	"strings"
)

type Kind string

const (
	Permanent  Kind = "permanent"
	DailyQuota Kind = "daily_quota"
	Throttle   Kind = "throttle"
	Transient  Kind = "transient"
	// Cancelled: the file's own active byte-upload transfer was
	// interrupted by a specific "skip this file" request (not a throttle,
	// not the whole run stopping) -- see uploader.FileCancelRegistry. A
	// distinct kind so it lands in the ledger's default failed_retryable
	// handling directly, without being retried in place (unlike
	// Transient) or held for the run-wide breaker (unlike Throttle).
	Cancelled Kind = "cancelled"
)

type Classification struct {
	Kind          Kind
	RetryAfter    float64
	HasRetryAfter bool
	Message       string
}

// APIError mirrors the shape of a Google API JSON error body's "error" object.
type APIError struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

func ClassifyResponse(statusCode int, apiErr *APIError, retryAfterHeader string) Classification {
	reason := ""
	message := ""
	if apiErr != nil {
		reason = strings.ToUpper(apiErr.Status)
		message = apiErr.Message
	}
	if message == "" {
		message = "HTTP error"
	}

	retryAfter, hasRetryAfter := parseRetryAfter(retryAfterHeader)

	if statusCode == 400 || reason == "INVALID_ARGUMENT" {
		return Classification{Kind: Permanent, Message: message}
	}
	if (statusCode == 401 || statusCode == 403) && (reason == "PERMISSION_DENIED" || reason == "UNAUTHENTICATED") {
		return Classification{Kind: Permanent, Message: message}
	}
	if statusCode == 429 || reason == "RESOURCE_EXHAUSTED" {
		lowered := strings.ToLower(message)
		// Only "per day" -- Google's real daily-exhaustion messages name the
		// limit explicitly ("...limit 'requests per day'"). A bare "daily"
		// substring used to be accepted here too, and that was the actual
		// false-positive trigger: transient per-minute/burst throttles have
		// been observed carrying wording like "...may affect your daily
		// usage patterns, please slow down", which matched "daily" and got
		// misread as genuine daily exhaustion -- abandoning the rest of a
		// run over what was really just a "slow down". "per day" doesn't
		// appear in that phrasing, so it cleanly separates the two.
		if strings.Contains(lowered, "per day") {
			return Classification{Kind: DailyQuota, RetryAfter: retryAfter, HasRetryAfter: hasRetryAfter, Message: message}
		}
		// Undocumented per-minute / concurrent-write throttle — reported by
		// real users (rclone issue trackers) but Google doesn't publish the
		// exact number.
		return Classification{Kind: Throttle, RetryAfter: retryAfter, HasRetryAfter: hasRetryAfter, Message: message}
	}
	if statusCode >= 500 {
		return Classification{Kind: Transient, RetryAfter: retryAfter, HasRetryAfter: hasRetryAfter, Message: message}
	}
	return Classification{Kind: Transient, RetryAfter: retryAfter, HasRetryAfter: hasRetryAfter, Message: message}
}

func parseRetryAfter(header string) (float64, bool) {
	if header == "" {
		return 0, false
	}
	seconds, err := strconv.ParseFloat(strings.TrimSpace(header), 64)
	if err != nil {
		return 0, false
	}
	return seconds, true
}

// BackoffDelay returns an exponential backoff delay with full jitter, in seconds.
func BackoffDelay(attempt int, base, cap float64) float64 {
	if base <= 0 {
		base = 1.0
	}
	if cap <= 0 {
		cap = 60.0
	}
	ceiling := base
	for i := 0; i < attempt; i++ {
		ceiling *= 2
		if ceiling >= cap {
			ceiling = cap
			break
		}
	}
	// #nosec G404 -- retry jitter only; nothing here is security sensitive,
	// and math/rand keeps this allocation-free on a hot retry path.
	return rand.Float64() * ceiling
}
