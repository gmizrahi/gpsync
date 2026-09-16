package retryx

import "testing"

func TestClassifyResponse_InvalidArgumentIsPermanent(t *testing.T) {
	c := ClassifyResponse(400, &APIError{Status: "INVALID_ARGUMENT", Message: "bad file"}, "")
	if c.Kind != Permanent {
		t.Errorf("got %q, want %q", c.Kind, Permanent)
	}
}

func TestClassifyResponse_PermissionDeniedIsPermanent(t *testing.T) {
	c := ClassifyResponse(403, &APIError{Status: "PERMISSION_DENIED", Message: "no scope"}, "")
	if c.Kind != Permanent {
		t.Errorf("got %q, want %q", c.Kind, Permanent)
	}
}

func TestClassifyResponse_DailyQuotaMessage(t *testing.T) {
	c := ClassifyResponse(429, &APIError{Status: "RESOURCE_EXHAUSTED", Message: "Quota exceeded for quota metric 'requests per day'"}, "")
	if c.Kind != DailyQuota {
		t.Errorf("got %q, want %q", c.Kind, DailyQuota)
	}
}

func TestClassifyResponse_ThrottleWhenNotDaily(t *testing.T) {
	c := ClassifyResponse(429, &APIError{Status: "RESOURCE_EXHAUSTED", Message: "concurrent write request limit exceeded"}, "")
	if c.Kind != Throttle {
		t.Errorf("got %q, want %q", c.Kind, Throttle)
	}
}

func TestClassifyResponse_ServerErrorIsTransient(t *testing.T) {
	c := ClassifyResponse(503, &APIError{Message: "backend unavailable"}, "")
	if c.Kind != Transient {
		t.Errorf("got %q, want %q", c.Kind, Transient)
	}
}

func TestClassifyResponse_ParsesRetryAfterHeader(t *testing.T) {
	c := ClassifyResponse(429, &APIError{Status: "RESOURCE_EXHAUSTED", Message: "throttled"}, "12.5")
	if !c.HasRetryAfter || c.RetryAfter != 12.5 {
		t.Errorf("got RetryAfter=%v HasRetryAfter=%v, want 12.5/true", c.RetryAfter, c.HasRetryAfter)
	}
}

func TestBackoffDelay_NeverExceedsCap(t *testing.T) {
	for attempt := 0; attempt < 10; attempt++ {
		d := BackoffDelay(attempt, 1.0, 5.0)
		if d < 0 || d > 5.0 {
			t.Errorf("attempt %d: delay %v out of [0,5]", attempt, d)
		}
	}
}

// TestClassifyResponse_DailyWordingInAThrottleIsNotDailyQuota proves the
// fix for the original false positive: this classifier used to accept a
// bare "daily" substring, so a transient per-minute/burst throttle whose
// text merely mentions "daily usage patterns" was reported as genuine
// daily-quota exhaustion -- which abandons the rest of a run's uploads
// instead of backing off and continuing. Only the explicit "per day"
// wording Google uses for a real daily limit counts now.
func TestClassifyResponse_DailyWordingInAThrottleIsNotDailyQuota(t *testing.T) {
	cases := []struct {
		name    string
		message string
		want    Kind
	}{
		{
			name:    "throttle mentioning daily usage patterns",
			message: "Quota exceeded, this may affect your daily usage patterns, please slow down",
			want:    Throttle,
		},
		{
			name:    "concurrent write throttle",
			message: "Quota exceeded for quota 'concurrent write request' of service 'photoslibrary.googleapis.com'",
			want:    Throttle,
		},
		{
			name:    "real daily exhaustion names the per-day limit",
			message: "Quota exceeded for quota metric 'requests' and limit 'requests per day'",
			want:    DailyQuota,
		},
		{
			name:    "per day wording is matched case-insensitively",
			message: "QUOTA EXCEEDED FOR LIMIT 'REQUESTS PER DAY'",
			want:    DailyQuota,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyResponse(429, &APIError{Status: "RESOURCE_EXHAUSTED", Message: tc.message}, "")
			if got.Kind != tc.want {
				t.Errorf("ClassifyResponse(%q).Kind = %q, want %q", tc.message, got.Kind, tc.want)
			}
		})
	}
}
