package engine

import (
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// API usage (issue #76): what the HTTP API served, by token name, endpoint
// and result, per day -- never which files.

// APIUsageKeepDays is how long daily usage is kept.
const APIUsageKeepDays = 90

// The endpoints usage is counted by.
const (
	APIEndpointList     = "list"
	APIEndpointThumb    = "thumb"
	APIEndpointOriginal = "original"
	APIEndpointToken    = "token"
	APIEndpointRotate   = "rotate"
	APIEndpointControl  = "control"
	APIEndpointOther    = "other" // no such route
)

// Results besides an HTTP status code: a request that succeeded, and for a
// thumbnail whether it came from the cache, was made for this request, or
// was already in the client's own cache (304).
const (
	APIResultOK        = "ok"
	APIResultCache     = "cache"
	APIResultMade      = "made"
	APIResultUnchanged = "unchanged"
)

// APIUsageCLIName is what the per-command tokens gpsync's own CLI mints are
// counted as: their names are random and they last one command. A token
// name can't contain a space, so it can't collide with a real one.
const APIUsageCLIName = "gpsync CLI"

// apiUsageFlushEvery is how often counts reach the ledger: often enough for
// Statistics to be current, without a ledger write per request.
const apiUsageFlushEvery = time.Minute

type apiUsageKey struct{ day, token, endpoint, result string }

type apiUsageCounts struct{ requests, bytes int64 }

// APIUsageCounter counts requests in memory and adds them to the ledger
// about once a minute -- on the first request after that, and whenever
// Flush is called (Statistics does, before reading).
type APIUsageCounter struct {
	mu        sync.Mutex
	counts    map[apiUsageKey]apiUsageCounts
	lastFlush time.Time
	now       func() time.Time
}

func NewAPIUsageCounter() *APIUsageCounter {
	return &APIUsageCounter{counts: map[apiUsageKey]apiUsageCounts{}, now: time.Now}
}

// Record counts one request, and flushes if a minute has passed. A failed
// flush keeps the counts for the next one.
func (c *APIUsageCounter) Record(db *statedb.DB, token, endpoint, result string, bytes int64) {
	c.mu.Lock()
	now := c.now()
	k := apiUsageKey{now.Local().Format(time.DateOnly), token, endpoint, result}
	v := c.counts[k]
	v.requests++
	v.bytes += bytes
	c.counts[k] = v
	due := now.Sub(c.lastFlush) >= apiUsageFlushEvery
	c.mu.Unlock()
	if due {
		_ = c.Flush(db)
	}
}

// Flush adds the counts so far to the ledger and drops days older than
// APIUsageKeepDays.
func (c *APIUsageCounter) Flush(db *statedb.DB) error {
	c.mu.Lock()
	pending := c.counts
	c.counts = map[apiUsageKey]apiUsageCounts{}
	now := c.now()
	c.lastFlush = now
	c.mu.Unlock()

	rows := make([]statedb.APIUsageRow, 0, len(pending))
	for k, v := range pending {
		rows = append(rows, statedb.APIUsageRow{Day: k.day, TokenName: k.token, Endpoint: k.endpoint, Result: k.result, Requests: v.requests, Bytes: v.bytes})
	}
	if err := db.AddAPIUsage(rows); err != nil {
		c.mu.Lock()
		for k, v := range pending { // put them back, onto anything counted since
			cur := c.counts[k]
			cur.requests += v.requests
			cur.bytes += v.bytes
			c.counts[k] = cur
		}
		c.mu.Unlock()
		return err
	}
	return db.PruneAPIUsage(now.Local().AddDate(0, 0, -APIUsageKeepDays).Format(time.DateOnly))
}

// APIUsagePeriods are the periods Statistics offers, in days.
var APIUsagePeriods = []int{7, 30, 90}

// ParseAPIUsagePeriod maps a query value to one of APIUsagePeriods,
// defaulting to 30 days.
func ParseAPIUsagePeriod(s string) int {
	for _, d := range APIUsagePeriods {
		if strconv.Itoa(d) == s {
			return d
		}
	}
	return 30
}

// APIUsageReport is the Statistics card and `gpsync stats --api`.
type APIUsageReport struct {
	Days                     int
	Requests, Bytes, Refused int64
	LastRequest              time.Time // zero: none
	Tokens                   []APITokenUsage
	Served                   []APIServedRow
	Refusals                 []APIRefusal
	CacheDir                 string
	CacheBytes, CacheMax     int64
	CacheErr                 error // the thumbnail folder setting is invalid
}

type APITokenUsage struct {
	Name      string
	Requests  int64
	LastUsed  time.Time // zero: never
	ExpiresAt time.Time // zero: never, or not a stored token
	Stored    bool      // false for APIUsageCLIName
}

type APIServedRow struct {
	Label           string
	Requests, Bytes int64
	Sub             bool // a breakdown of the row above
}

type APIRefusal struct {
	Status   string
	Reason   string
	Requests int64
}

var apiRefusalReasons = map[string]string{
	"400": "Request the API couldn't use",
	"401": "Bad, expired or revoked token",
	"403": "Not from this PC, or missing scope",
	"404": "Unknown file or route, or original not available",
	"405": "Wrong method",
	"415": "No thumbnail for this format",
	"429": "Too many failures",
	"500": "Error inside gpsync",
}

// BuildAPIUsageReport sums the last days of usage, including today. hasTokens
// reports whether any API token exists: Statistics shows the card only then.
func BuildAPIUsageReport(db *statedb.DB, cfg config.Config, days int, now time.Time) (r APIUsageReport, hasTokens bool, err error) {
	r.Days = days
	tokens, err := db.ListAPITokens()
	if err != nil {
		return r, false, err
	}
	rows, err := db.APIUsageSince(now.Local().AddDate(0, 0, 1-days).Format(time.DateOnly))
	if err != nil {
		return r, len(tokens) > 0, err
	}

	byToken := map[string]int64{}
	type served struct{ n, b int64 }
	s := map[string]served{}
	refused := map[string]int64{}
	for _, u := range rows {
		r.Requests += u.Requests
		if u.TokenName != "" {
			byToken[u.TokenName] += u.Requests
		}
		if _, err := strconv.Atoi(u.Result); err == nil {
			r.Refused += u.Requests
			refused[u.Result] += u.Requests
			continue
		}
		r.Bytes += u.Bytes
		key := u.Endpoint
		if u.Endpoint == APIEndpointThumb {
			key = u.Endpoint + "/" + u.Result
		}
		v := s[key]
		v.n += u.Requests
		v.b += u.Bytes
		s[key] = v
	}

	for _, t := range tokens {
		tu := APITokenUsage{Name: t.Name, Requests: byToken[t.Name], Stored: true}
		if t.LastUsedAt.Valid {
			tu.LastUsed = time.Unix(int64(t.LastUsedAt.Float64), 0)
			if tu.LastUsed.After(r.LastRequest) {
				r.LastRequest = tu.LastUsed
			}
		}
		if t.ExpiresAt.Valid {
			tu.ExpiresAt = time.Unix(int64(t.ExpiresAt.Float64), 0)
		}
		r.Tokens = append(r.Tokens, tu)
	}
	if n := byToken[APIUsageCLIName]; n > 0 {
		r.Tokens = append(r.Tokens, APITokenUsage{Name: APIUsageCLIName, Requests: n})
	}

	add := func(label string, sub bool, keys ...string) {
		var v served
		for _, k := range keys {
			v.n += s[k].n
			v.b += s[k].b
		}
		if v.n > 0 {
			r.Served = append(r.Served, APIServedRow{Label: label, Requests: v.n, Bytes: v.b, Sub: sub})
		}
	}
	thumbs := []string{APIEndpointThumb + "/" + APIResultCache, APIEndpointThumb + "/" + APIResultMade, APIEndpointThumb + "/" + APIResultUnchanged}
	add("Lists", false, APIEndpointList)
	add("Thumbnails", false, thumbs...)
	add("from the cache", true, thumbs[0])
	add("made for the request", true, thumbs[1])
	add("unchanged, the program had it", true, thumbs[2])
	add("Originals", false, APIEndpointOriginal)
	add("Token checks and rotations", false, APIEndpointToken, APIEndpointRotate)
	add("Tray control", false, APIEndpointControl)

	for status, n := range refused {
		reason, ok := apiRefusalReasons[status]
		if !ok {
			reason = "Refused"
		}
		r.Refusals = append(r.Refusals, APIRefusal{Status: status, Reason: reason, Requests: n})
	}
	sort.Slice(r.Refusals, func(i, j int) bool {
		if r.Refusals[i].Requests != r.Refusals[j].Requests {
			return r.Refusals[i].Requests > r.Refusals[j].Requests
		}
		return r.Refusals[i].Status < r.Refusals[j].Status
	})

	r.CacheMax = ThumbCacheMaxBytes
	if r.CacheDir, r.CacheErr = ThumbnailDir(cfg); r.CacheErr == nil {
		for _, e := range thumbEntries(r.CacheDir) {
			r.CacheBytes += e.size
		}
	}
	return r, len(tokens) > 0, nil
}

// Label is how a refusal reads: its reason and status code.
func (a APIRefusal) Label() string { return fmt.Sprintf("%s (%s)", a.Reason, a.Status) }
