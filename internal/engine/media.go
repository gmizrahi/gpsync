package engine

import (
	"encoding/base64"
	"errors"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gmizrahi/gpsync/internal/extensions"
	"github.com/gmizrahi/gpsync/internal/preprocess"
	"github.com/gmizrahi/gpsync/internal/scanner"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// The media API (issue #66): a read-only view of the files gpsync knows, for
// another program on this machine to show them. Nothing here writes to the
// ledger.

const (
	MediaListDefaultLimit = 200
	MediaListMaxLimit     = 1000
)

var (
	// ErrMediaNotFound: no listable file has this hash -- unknown, ignored,
	// waiting for review, or confirmed missing.
	ErrMediaNotFound = errors.New("no such media")
	// ErrMediaUnavailable: the file is known, but no scanned copy of it is
	// still where it was, unchanged.
	ErrMediaUnavailable = errors.New("the original is not available")
	// ErrNoThumbnail: the file's format has no thumbnail (video, HEIC, RAW).
	ErrNoThumbnail = errors.New("no thumbnail for this format")
	// ErrBadCursor: a list cursor that this API didn't issue.
	ErrBadCursor = errors.New("invalid cursor")
)

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidSHA256 reports whether s is a content hash as the ledger writes it:
// 64 lowercase hex digits.
func ValidSHA256(s string) bool { return sha256Pattern.MatchString(s) }

// MediaItem is one listed file. CapturedAt is in the camera's own zone when
// it recorded one (Offset), else this machine's (see docs/api.md for what it
// means and where it comes from). The rest is set only when known.
type MediaItem struct {
	SHA256        string
	CapturedAt    time.Time
	CapturedFrom  string          // scanner.CaptureFromEXIF, ...Path, ...Mtime, or "" if unknown
	Offset        string          // like "+02:00"
	Kind          extensions.Kind // photo or video; empty if the extension is no longer a known one
	MimeType      string
	HasThumb      bool
	Lat, Lon      *float64
	Width, Height *int
}

// MediaQuery is a list request: files captured in [From, To), zero meaning
// unbounded, after Cursor.
type MediaQuery struct {
	From, To time.Time
	Limit    int
	Cursor   string
}

// ListMedia returns one page of files in capture order and the cursor for the
// next page, "" on the last one.
func ListMedia(db *statedb.DB, q MediaQuery) (items []MediaItem, next string, err error) {
	from, to := math.Inf(-1), math.Inf(1)
	if !q.From.IsZero() {
		from = unixSeconds(q.From)
	}
	if !q.To.IsZero() {
		to = unixSeconds(q.To)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = MediaListDefaultLimit
	}
	limit = min(limit, MediaListMaxLimit)
	var afterAt float64
	var afterSHA string
	if q.Cursor != "" {
		if afterAt, afterSHA, err = decodeMediaCursor(q.Cursor); err != nil {
			return nil, "", err
		}
	}
	// One more than asked: whether it comes back says whether there's a
	// next page, without a second query.
	rows, err := db.ListMedia(from, to, afterAt, afterSHA, limit+1)
	if err != nil {
		return nil, "", err
	}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		next = encodeMediaCursor(last.CapturedAt, last.SHA256)
	}
	items = make([]MediaItem, 0, len(rows))
	for _, r := range rows {
		it := MediaItem{
			SHA256:       r.SHA256,
			CapturedAt:   fromUnixSeconds(r.CapturedAt),
			CapturedFrom: r.CaptureSource.String,
			Kind:         mediaKind(r),
			MimeType:     r.MimeType,
			HasThumb:     preprocess.IsResizable(r.FirstSourcePath),
		}
		if zone, ok := offsetZone(r.CaptureOffset.String); ok && it.CapturedFrom == scanner.CaptureFromEXIF {
			it.CapturedAt, it.Offset = it.CapturedAt.In(zone), r.CaptureOffset.String
		}
		if r.Lat.Valid && r.Lon.Valid {
			it.Lat, it.Lon = &r.Lat.Float64, &r.Lon.Float64
		}
		if r.Width.Valid && r.Height.Valid {
			w, h := int(r.Width.Int64), int(r.Height.Int64)
			it.Width, it.Height = &w, &h
		}
		items = append(items, it)
	}
	return items, next, nil
}

// mediaKind is the file's kind by its extension, or failing that by the
// MIME type recorded when it was scanned.
func mediaKind(r statedb.MediaRow) extensions.Kind {
	if k := extensions.KindOf(r.FirstSourcePath); k != extensions.KindUnknown {
		return k
	}
	switch {
	case strings.HasPrefix(r.MimeType, "image/"):
		return extensions.KindPhoto
	case strings.HasPrefix(r.MimeType, "video/"):
		return extensions.KindVideo
	}
	return extensions.KindUnknown
}

// offsetZone turns a recorded offset like "+02:00" into a zone.
func offsetZone(o string) (*time.Location, bool) {
	t, err := time.Parse("-07:00", o)
	if err != nil {
		return nil, false
	}
	_, secs := t.Zone()
	return time.FixedZone(o, secs), true
}

func unixSeconds(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

func fromUnixSeconds(s float64) time.Time {
	sec, frac := math.Modf(s)
	return time.Unix(int64(sec), int64(frac*1e9)).Local()
}

// The cursor is the last row's (captured_at, sha256). It's opaque to callers
// but not secret -- base64 so it reads as a token, not as something to edit.
// 'g' with precision -1 round-trips the float exactly, which the keyset
// comparison needs.
func encodeMediaCursor(at float64, sha string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatFloat(at, 'g', -1, 64) + "|" + sha))
}

func decodeMediaCursor(c string) (float64, string, error) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return 0, "", ErrBadCursor
	}
	atText, sha, ok := strings.Cut(string(b), "|")
	at, perr := strconv.ParseFloat(atText, 64)
	if !ok || perr != nil || math.IsNaN(at) || math.IsInf(at, 0) || !ValidSHA256(sha) {
		return 0, "", ErrBadCursor
	}
	return at, sha, nil
}

// LocateMedia finds a listable file's original on disk: a scanned path that
// held this content and whose size and modification time are still the ones
// recorded when it was hashed -- the scanner's own test for "unchanged".
// That is what lets a thumbnail, cached by content hash, never be made from
// a file that has since been edited or replaced. A file that changed is
// unavailable until the next scan hashes it again.
func LocateMedia(db *statedb.DB, sha string) (statedb.MediaRow, string, error) {
	m, err := db.GetListableMedia(sha)
	if err != nil {
		return statedb.MediaRow{}, "", err
	}
	if m == nil {
		return statedb.MediaRow{}, "", ErrMediaNotFound
	}
	seen, err := db.FilesSeenWithSHA(sha)
	if err != nil {
		return *m, "", err
	}
	// Any unchanged copy will do: they all hold the same bytes.
	for _, f := range seen {
		if info, err := os.Stat(f.Path); err == nil && info.Mode().IsRegular() && f.Unchanged(info) {
			return *m, f.Path, nil
		}
	}
	return *m, "", ErrMediaUnavailable
}
