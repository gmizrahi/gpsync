package scanner

import (
	"bytes"
	"image"
	_ "image/gif" // DecodeConfig: dimensions from the header
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"os"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/rwcarlsen/goexif/exif"
	"github.com/rwcarlsen/goexif/tiff"
	_ "golang.org/x/image/webp"

	"github.com/gmizrahi/gpsync/internal/extensions"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// Where a capture time came from, recorded as uploads.capture_source so a
// caller matching photos by time knows how far to trust it.
const (
	CaptureFromEXIF  = "exif"
	CaptureFromPath  = "path"
	CaptureFromMtime = "mtime"
)

// Metadata is what a scan reads from a file besides its hash. Every field
// is set only when the file actually records it -- never a guess.
type Metadata struct {
	// CapturedAt is the capture time, Unix seconds: the EXIF date (at its
	// recorded offset, if any, else this machine's zone), else a date in
	// the path, else the modification time. Source says which.
	CapturedAt *float64
	Source     string
	// EXIFAsLocal is the EXIF date read in this machine's zone even when an
	// offset was recorded: what scans stored before offsets were read. A
	// backfill matches a stored capture time against it to tell whether
	// that time came from EXIF.
	EXIFAsLocal *float64
	// Offset is the camera's own time-zone offset, like "+02:00".
	Offset        string
	Lat, Lon      *float64
	Width, Height *int // as displayed: EXIF orientation applied
}

// exifCapableExts: goexif only decodes JPEG/TIFF-container EXIF. Everything
// else (PNG, HEIC, RAW, video) falls back to a date in the path, then mtime.
var exifCapableExts = map[string]bool{"jpg": true, "jpeg": true, "tif": true, "tiff": true}

// headerDimensionExts are the formats image.DecodeConfig can size from the
// header alone, with the decoders imported above.
var headerDimensionExts = map[string]bool{"jpg": true, "jpeg": true, "png": true, "gif": true, "webp": true}

// The offset tags (0x9010 OffsetTime, 0x9011 OffsetTimeOriginal) postdate
// goexif's field table, and it drops tags it doesn't know from the EXIF
// sub-IFD. offsetParser loads them through goexif's own parser hook.
const (
	offsetTime         exif.FieldName = "OffsetTime"
	offsetTimeOriginal exif.FieldName = "OffsetTimeOriginal"
)

type offsetParser struct{}

func (offsetParser) Parse(x *exif.Exif) error {
	tag, err := x.Get(exif.ExifIFDPointer)
	if err != nil {
		return nil
	}
	off, err := tag.Int64(0)
	if err != nil {
		return nil
	}
	r := bytes.NewReader(x.Raw)
	if _, err := r.Seek(off, 0); err != nil {
		return nil
	}
	dir, _, err := tiff.DecodeDir(r, x.Tiff.Order)
	if err != nil {
		return nil
	}
	x.LoadTags(dir, map[uint16]exif.FieldName{0x9010: offsetTime, 0x9011: offsetTimeOriginal}, false)
	return nil
}

func init() { exif.RegisterParsers(offsetParser{}) }

var offsetPattern = regexp.MustCompile(`^[+-](0\d|1[0-4]):[0-5]\d$`)

// ReadMetadata reads what a scan records about the file at path. It reads
// only the EXIF block and the image header, never the whole file.
func ReadMetadata(path string) Metadata {
	var m Metadata
	ext := extensions.ExtOf(path)
	orientation := 1
	var exifW, exifH int
	// One open for both reads: EXIF, then, from the start again, the image
	// header for the real dimensions.
	w, h := 0, 0
	if exifCapableExts[ext] || headerDimensionExts[ext] {
		if f, err := os.Open(path); err == nil {
			if exifCapableExts[ext] {
				// A critical error leaves x nil; a non-critical one (a bad
				// GPS or interop sub-IFD) still returns the rest.
				if x, err := exif.Decode(f); x != nil && (err == nil || !exif.IsCriticalError(err)) {
					readEXIFDate(x, &m)
					if lat, lon, err := x.LatLong(); err == nil && validCoordinates(lat, lon) {
						m.Lat, m.Lon = &lat, &lon
					}
					orientation = exifInt(x, exif.Orientation, 1)
					exifW, exifH = exifInt(x, exif.PixelXDimension, 0), exifInt(x, exif.PixelYDimension, 0)
				}
			}
			if headerDimensionExts[ext] {
				if _, err := f.Seek(0, io.SeekStart); err == nil {
					if cfg, _, err := image.DecodeConfig(f); err == nil {
						w, h = cfg.Width, cfg.Height
					}
				}
			}
			f.Close()
		}
	}
	if m.CapturedAt == nil {
		if t, ok := DateFromPath(path); ok {
			ts := float64(t.Unix())
			m.CapturedAt, m.Source = &ts, CaptureFromPath
		} else if info, err := os.Stat(path); err == nil {
			ts := float64(info.ModTime().Unix())
			m.CapturedAt, m.Source = &ts, CaptureFromMtime
		}
	}

	if w <= 0 || h <= 0 {
		w, h = exifW, exifH
	}
	if w > 0 && h > 0 {
		if orientation >= 5 && orientation <= 8 { // rotated a quarter turn
			w, h = h, w
		}
		m.Width, m.Height = &w, &h
	}
	return m
}

// readEXIFDate takes DateTimeOriginal with OffsetTimeOriginal, or failing
// that DateTime with OffsetTime: each offset belongs to its own date.
func readEXIFDate(x *exif.Exif, m *Metadata) {
	for _, pair := range [][2]exif.FieldName{{exif.DateTimeOriginal, offsetTimeOriginal}, {exif.DateTime, offsetTime}} {
		tag, err := x.Get(pair[0])
		if err != nil {
			continue
		}
		s, err := tag.StringVal()
		if err != nil {
			continue
		}
		local, err := time.ParseInLocation("2006:01:02 15:04:05", s, time.Local)
		if err != nil {
			continue
		}
		asLocal := float64(local.Unix())
		at := asLocal
		if ot, err := x.Get(pair[1]); err == nil {
			if o, err := ot.StringVal(); err == nil && offsetPattern.MatchString(o) {
				zoned, _ := time.Parse("2006:01:02 15:04:05-07:00", s+o)
				at = float64(zoned.Unix())
				m.Offset = o
			}
		}
		m.CapturedAt, m.EXIFAsLocal, m.Source = &at, &asLocal, CaptureFromEXIF
		return
	}
}

// validCoordinates refuses out-of-range values and exactly 0,0: cameras
// without a fix write zeros, and a photo really taken at that point in the
// Gulf of Guinea is rarer than one mislabelled there.
func validCoordinates(lat, lon float64) bool {
	if math.IsNaN(lat) || math.IsNaN(lon) || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return false
	}
	return lat != 0 || lon != 0
}

func exifInt(x *exif.Exif, name exif.FieldName, fallback int) int {
	tag, err := x.Get(name)
	if err != nil {
		return fallback
	}
	if v, err := tag.Int(0); err == nil {
		return v
	}
	if s, err := tag.StringVal(); err == nil {
		if v, err := strconv.Atoi(s); err == nil {
			return v
		}
	}
	return fallback
}

// recordMetadata stores what ReadMetadata found for a row, once (see
// statedb.RecordMediaMetadata), deciding what the row's stored capture time
// may be said to have come from:
//
//   - no stored time: take this one, and its source;
//   - the stored time is this file's own derivation: its source;
//   - it is the EXIF date read in this machine's zone, from a scan before
//     offsets were read: EXIF -- and, when an offset is known, correct it
//     to the instant the camera recorded;
//   - anything else (fix-dates changed it, or the rules changed since):
//     source unknown, and the stored time is kept.
func recordMetadata(db *statedb.DB, sha string, m Metadata, now float64) error {
	u, err := db.GetUpload(sha)
	if err != nil || u == nil {
		return err
	}
	rec := statedb.MediaMetadata{Offset: m.Offset, Lat: m.Lat, Lon: m.Lon, Width: m.Width, Height: m.Height}
	switch stored := u.CapturedAt; {
	case !stored.Valid:
		rec.CapturedAt, rec.Source = m.CapturedAt, m.Source
	case m.CapturedAt != nil && stored.Float64 == *m.CapturedAt:
		rec.Source = m.Source
	case m.Source == CaptureFromEXIF && m.EXIFAsLocal != nil && stored.Float64 == *m.EXIFAsLocal:
		rec.CapturedAt, rec.Source = m.CapturedAt, CaptureFromEXIF
	}
	_, err = db.RecordMediaMetadata(sha, rec, now)
	return err
}

// backfillMetadata reads metadata for unchanged files whose rows predate it:
// a scan does this for the files it would otherwise skip, so an existing
// library fills in over ordinary scans, resumably -- each row is marked
// once read -- and alongside a running tray, through the ledger's usual
// locking. Only EXIF blocks and image headers are read.
func backfillMetadata(db *statedb.DB, cacheHits []*statedb.FileSeen, concurrency int) (int, error) {
	missing, err := db.SHA256sWithoutMetadata()
	if err != nil || len(missing) == 0 {
		return 0, err
	}
	type result struct {
		sha string
		m   Metadata
	}
	jobs := make(chan *statedb.FileSeen)
	results := make(chan result)
	var wg sync.WaitGroup
	for range max(1, concurrency) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range jobs {
				results <- result{f.SHA256, ReadMetadata(f.Path)}
			}
		}()
	}
	go func() {
		for _, f := range cacheHits {
			if missing[f.SHA256] {
				delete(missing, f.SHA256) // one copy per content
				jobs <- f
			}
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()
	now := float64(time.Now().UnixNano()) / 1e9
	var firstErr error
	read := 0
	for r := range results {
		if err := recordMetadata(db, r.sha, r.m, now); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		read++
	}
	return read, firstErr
}
