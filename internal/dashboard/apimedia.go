package dashboard

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// The media API (issue #66), read-only: the list, a thumbnail, the original.
// See docs/api.md for the contract.

type mediaItemJSON struct {
	SHA256     string `json:"sha256"`
	CapturedAt string `json:"captured_at"` // RFC 3339, this machine's time zone
	Kind       string `json:"kind"`        // "photo", "video", or "" if unknown
	MimeType   string `json:"mime_type"`
	HasThumb   bool   `json:"has_thumb"`
}

type mediaListJSON struct {
	Items []mediaItemJSON `json:"items"`
	Next  *string         `json:"next"` // null on the last page
}

// handleMediaList serves GET /api/v1/media?from=&to=&limit=&cursor=.
func handleMediaList(w http.ResponseWriter, r *http.Request, db *statedb.DB) {
	query := r.URL.Query()
	var q engine.MediaQuery
	var err error
	for name, dst := range map[string]*time.Time{"from": &q.From, "to": &q.To} {
		if v := query.Get(name); v != "" {
			if *dst, err = time.Parse(time.RFC3339, v); err != nil {
				http.Error(w, name+" must be an RFC 3339 time, such as 2024-05-01T00:00:00+02:00", http.StatusBadRequest)
				return
			}
		}
	}
	if v := query.Get("limit"); v != "" {
		if q.Limit, err = strconv.Atoi(v); err != nil || q.Limit < 1 || q.Limit > engine.MediaListMaxLimit {
			http.Error(w, "limit must be a number from 1 to "+strconv.Itoa(engine.MediaListMaxLimit), http.StatusBadRequest)
			return
		}
	}
	q.Cursor = query.Get("cursor")
	items, next, err := engine.ListMedia(db, q)
	if errors.Is(err, engine.ErrBadCursor) {
		http.Error(w, "cursor must be a next value from an earlier page", http.StatusBadRequest)
		return
	}
	if err != nil {
		log.Printf("api: listing media failed: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := mediaListJSON{Items: make([]mediaItemJSON, 0, len(items))}
	for _, it := range items {
		out.Items = append(out.Items, mediaItemJSON{
			SHA256:     it.SHA256,
			CapturedAt: it.CapturedAt.Format(time.RFC3339),
			Kind:       string(it.Kind),
			MimeType:   it.MimeType,
			HasThumb:   it.HasThumb,
		})
	}
	if next != "" {
		out.Next = &next
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// mediaSHA reads and checks the {sha256} path segment, answering 400 itself
// when it isn't a content hash.
func mediaSHA(w http.ResponseWriter, r *http.Request) (string, bool) {
	sha := r.PathValue("sha256")
	if !engine.ValidSHA256(sha) {
		http.Error(w, "not a SHA-256 content hash (64 lowercase hex digits)", http.StatusBadRequest)
		return "", false
	}
	return sha, true
}

// locateOrFail finds the original, answering 404 itself when the file is
// unknown, not listed, or no longer where it was, unchanged.
func locateOrFail(w http.ResponseWriter, r *http.Request, db *statedb.DB, sha string) (statedb.MediaRow, string, bool) {
	m, path, err := engine.LocateMedia(db, sha)
	switch {
	case errors.Is(err, engine.ErrMediaNotFound):
		http.Error(w, "no such media", http.StatusNotFound)
	case errors.Is(err, engine.ErrMediaUnavailable):
		http.Error(w, "the original is not available", http.StatusNotFound)
	case err != nil:
		log.Printf("api: locating media failed: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	default:
		return m, path, true
	}
	return m, "", false
}

// cacheForever marks a response as never changing: everything here is
// addressed by content hash.
func cacheForever(w http.ResponseWriter, etag string) {
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("ETag", etag)
}

// handleMediaThumb serves GET /api/v1/media/{sha256}/thumb?size=.
func handleMediaThumb(w http.ResponseWriter, r *http.Request, db *statedb.DB, thumbs *engine.ThumbCache) {
	sha, ok := mediaSHA(w, r)
	if !ok {
		return
	}
	size := 0
	if v := r.URL.Query().Get("size"); v != "" {
		var err error
		if size, err = strconv.Atoi(v); err != nil || size < 1 {
			http.Error(w, "size must be a positive number of pixels", http.StatusBadRequest)
			return
		}
	}
	size = engine.SnapThumbSize(size)
	_, path, ok := locateOrFail(w, r, db, sha)
	if !ok {
		return
	}
	data, err := thumbs.Thumbnail(sha, path, size)
	switch {
	case errors.Is(err, engine.ErrNoThumbnail):
		http.Error(w, "this format has no thumbnail", http.StatusUnsupportedMediaType)
		return
	case err != nil:
		http.Error(w, "the original is not available", http.StatusNotFound)
		return
	}
	cacheForever(w, `"`+sha+"-"+strconv.Itoa(size)+`"`)
	w.Header().Set("Content-Type", "image/jpeg")
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
}

// handleMediaOriginal serves GET /api/v1/media/{sha256}: the file's bytes,
// with ranges, so a video can be streamed.
func handleMediaOriginal(w http.ResponseWriter, r *http.Request, db *statedb.DB) {
	sha, ok := mediaSHA(w, r)
	if !ok {
		return
	}
	m, path, ok := locateOrFail(w, r, db, sha)
	if !ok {
		return
	}
	// #nosec G304 -- path is not request input: LocateMedia returned a path
	// the scanner recorded for this hash.
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "the original is not available", http.StatusNotFound)
		return
	}
	defer f.Close()
	contentType := m.MimeType
	if contentType == "" {
		contentType = mime.TypeByExtension(filepath.Ext(path))
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	cacheForever(w, `"`+sha+`"`)
	w.Header().Set("Content-Type", contentType)
	http.ServeContent(w, r, "", time.Time{}, f)
}
