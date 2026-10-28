package dashboard

import (
	"fmt"
	"html/template"
	"time"

	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/units"
)

// apiUsageView is the Statistics "API usage" card (#76).
type apiUsageView struct {
	Periods                                 []apiPeriodTab
	Requests, Served, Refused, LastRequest  string
	Tokens                                  []apiTokenUsageRow
	ServedRows                              []apiServedRow
	CacheDir, CacheUsed, CacheMax, CacheErr string
	CacheBarStyle                           template.CSS
	Refusals                                []apiRefusalRow
	KeepDays                                int
}

type apiPeriodTab struct {
	Label, Href string
	Active      bool
}

type apiTokenUsageRow struct{ Name, Requests, LastUsed, Expires string }

type apiServedRow struct {
	Label, Count, Size string
	Sub                bool
}

type apiRefusalRow struct{ Label, Count string }

func buildAPIUsageView(r engine.APIUsageReport, activity engine.ActivityGranularity, now time.Time) *apiUsageView {
	v := &apiUsageView{
		Requests:    humanCount(int(r.Requests)),
		Served:      humanBytes(r.Bytes),
		Refused:     humanCount(int(r.Refused)),
		LastRequest: "never",
		KeepDays:    engine.APIUsageKeepDays,
	}
	for _, d := range engine.APIUsagePeriods {
		v.Periods = append(v.Periods, apiPeriodTab{
			Label:  fmt.Sprintf("%d days", d),
			Href:   fmt.Sprintf("/statistics?activity=%s&api=%d#api-usage", activity, d),
			Active: d == r.Days,
		})
	}
	if !r.LastRequest.IsZero() {
		v.LastRequest = units.Relative(r.LastRequest, now)
	}
	for _, t := range r.Tokens {
		row := apiTokenUsageRow{Name: t.Name, Requests: humanCount(int(t.Requests)), LastUsed: "never", Expires: "never"}
		if !t.LastUsed.IsZero() {
			row.LastUsed = units.Relative(t.LastUsed, now)
		}
		switch {
		case !t.Stored:
			row.LastUsed, row.Expires = "—", "—" // minted per command, gone after it
		case !t.ExpiresAt.IsZero() && !t.ExpiresAt.After(now):
			row.Expires = "expired"
		case !t.ExpiresAt.IsZero():
			row.Expires = units.Relative(t.ExpiresAt, now)
		}
		v.Tokens = append(v.Tokens, row)
	}
	for _, s := range r.Served {
		row := apiServedRow{Label: s.Label, Count: humanCount(int(s.Requests)), Size: humanBytes(s.Bytes), Sub: s.Sub}
		if s.Bytes == 0 {
			row.Size = "—"
		}
		v.ServedRows = append(v.ServedRows, row)
	}
	for _, f := range r.Refusals {
		v.Refusals = append(v.Refusals, apiRefusalRow{Label: f.Label(), Count: humanCount(int(f.Requests))})
	}
	if r.CacheErr != nil {
		v.CacheErr = "Thumbnail folder setting is invalid: " + r.CacheErr.Error()
	} else {
		v.CacheDir, v.CacheUsed, v.CacheMax = r.CacheDir, humanBytes(r.CacheBytes), humanBytes(r.CacheMax)
		pct := 0.0
		if r.CacheMax > 0 {
			pct = min(100, float64(r.CacheBytes)*100/float64(r.CacheMax))
		}
		v.CacheBarStyle = template.CSS(fmt.Sprintf("width:%.1f%%", pct))
	}
	return v
}
