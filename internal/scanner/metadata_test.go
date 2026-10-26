package scanner

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"image"
	"image/jpeg"
	"image/png"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/gmizrahi/gpsync/internal/statedb"
)

// exifEntry is one IFD entry for the test EXIF writer: a tag, its TIFF type
// (2 ASCII, 3 SHORT, 4 LONG, 5 RATIONAL) and its value bytes.
type exifEntry struct {
	tag, typ uint16
	count    uint32
	data     []byte
}

func ascii(s string) exifEntry {
	return exifEntry{typ: 2, count: uint32(len(s) + 1), data: append([]byte(s), 0)}
}

func short(v uint16) exifEntry {
	return exifEntry{typ: 3, count: 1, data: binary.LittleEndian.AppendUint16(nil, v)}
}

func long(v uint32) exifEntry {
	return exifEntry{typ: 4, count: 1, data: binary.LittleEndian.AppendUint32(nil, v)}
}

// dms is a coordinate as three rationals: degrees, minutes, seconds*100/100.
func dms(v float64) exifEntry {
	v = math.Abs(v)
	d := math.Floor(v)
	m := math.Floor((v - d) * 60)
	s := ((v-d)*60 - m) * 60
	var b []byte
	for _, r := range [][2]uint32{{uint32(d), 1}, {uint32(m), 1}, {uint32(math.Round(s * 1000)), 1000}} {
		b = binary.LittleEndian.AppendUint32(b, r[0])
		b = binary.LittleEndian.AppendUint32(b, r[1])
	}
	return exifEntry{typ: 5, count: 3, data: b}
}

func with(tag uint16, e exifEntry) exifEntry { e.tag = tag; return e }

// tiffWithIFDs lays out IFD0, then the EXIF and GPS sub-IFDs (if any), each
// followed by its out-of-line values, and points IFD0 at the other two.
func tiffWithIFDs(ifd0, exifIFD, gpsIFD []exifEntry) []byte {
	buf := []byte("II*\x00\x08\x00\x00\x00")
	ifdSize := func(es []exifEntry) int {
		n := 2 + 12*len(es) + 4
		for _, e := range es {
			if len(e.data) > 4 {
				n += len(e.data) + len(e.data)%2
			}
		}
		return n
	}
	pointers := []exifEntry{}
	at := 8 + ifdSize(ifd0) + 2*12 // room for up to two pointer entries
	if len(exifIFD) > 0 {
		pointers = append(pointers, with(0x8769, long(uint32(at))))
		at += ifdSize(exifIFD)
	}
	if len(gpsIFD) > 0 {
		pointers = append(pointers, with(0x8825, long(uint32(at))))
	}
	// IFD0's size was reserved for two pointers; pad if fewer are used.
	ifd0 = append(append([]exifEntry{}, ifd0...), pointers...)
	write := func(es []exifEntry, pad int) {
		start := len(buf)
		dataAt := start + 2 + 12*len(es) + 4 + pad
		buf = binary.LittleEndian.AppendUint16(buf, uint16(len(es)))
		var data []byte
		for _, e := range es {
			buf = binary.LittleEndian.AppendUint16(buf, e.tag)
			buf = binary.LittleEndian.AppendUint16(buf, e.typ)
			buf = binary.LittleEndian.AppendUint32(buf, e.count)
			if len(e.data) <= 4 {
				buf = append(buf, append(e.data, make([]byte, 4-len(e.data))...)...)
				continue
			}
			buf = binary.LittleEndian.AppendUint32(buf, uint32(dataAt+len(data)))
			data = append(data, e.data...)
			if len(e.data)%2 == 1 {
				data = append(data, 0)
			}
		}
		buf = append(buf, 0, 0, 0, 0) // no next IFD
		buf = append(buf, make([]byte, pad)...)
		buf = append(buf, data...)
	}
	write(ifd0, 12*(2-len(pointers)))
	if len(exifIFD) > 0 {
		write(exifIFD, 0)
	}
	if len(gpsIFD) > 0 {
		write(gpsIFD, 0)
	}
	return buf
}

// jpegWithEXIF is a real w x h JPEG with an APP1 EXIF segment.
func jpegWithEXIF(t *testing.T, w, h int, tiffData []byte) []byte {
	t.Helper()
	var img bytes.Buffer
	if err := jpeg.Encode(&img, image.NewGray(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatal(err)
	}
	payload := append([]byte("Exif\x00\x00"), tiffData...)
	seg := []byte{0xFF, 0xE1}
	seg = binary.BigEndian.AppendUint16(seg, uint16(len(payload)+2))
	return append(append([]byte{0xFF, 0xD8}, append(seg, payload...)...), img.Bytes()[2:]...)
}

func fullEXIF(dateTag, offsetTag uint16, date, offset string, lat, lon float64, orientation uint16) []byte {
	exifIFD := []exifEntry{with(dateTag, ascii(date))}
	if offset != "" {
		exifIFD = append(exifIFD, with(offsetTag, ascii(offset)))
	}
	exifIFD = append(exifIFD, with(0xA002, long(4000)), with(0xA003, long(3000)))
	latRef, lonRef := "N", "E"
	if lat < 0 {
		latRef = "S"
	}
	if lon < 0 {
		lonRef = "W"
	}
	gps := []exifEntry{with(1, ascii(latRef)), with(2, dms(lat)), with(3, ascii(lonRef)), with(4, dms(lon))}
	return tiffWithIFDs([]exifEntry{with(0x0112, short(orientation))}, exifIFD, gps)
}

func near(a *float64, b float64) bool { return a != nil && math.Abs(*a-b) < 1e-4 }

// Issue #68: what a scan reads from a photo -- the camera's offset with its
// own date, GPS, and the dimensions as displayed -- and nothing it doesn't
// record.
func TestReadMetadata(t *testing.T) {
	dir := t.TempDir()
	wall := "2024:05:01 09:30:00"
	local, _ := time.ParseInLocation("2006:01:02 15:04:05", wall, time.Local)
	zoned, _ := time.Parse("2006:01:02 15:04:05-07:00", wall+"-05:30")

	p := filepath.Join(dir, "a.jpg")
	writeFile(t, p, jpegWithEXIF(t, 40, 30, fullEXIF(0x9003, 0x9011, wall, "-05:30", -33.8675, 151.207, 6)))
	m := ReadMetadata(p)
	if m.Source != CaptureFromEXIF || m.Offset != "-05:30" || m.CapturedAt == nil || int64(*m.CapturedAt) != zoned.Unix() {
		t.Errorf("date: source %q, offset %q, at %v; want exif, -05:30, %d", m.Source, m.Offset, m.CapturedAt, zoned.Unix())
	}
	if m.EXIFAsLocal == nil || int64(*m.EXIFAsLocal) != local.Unix() {
		t.Errorf("EXIFAsLocal = %v, want %d", m.EXIFAsLocal, local.Unix())
	}
	if !near(m.Lat, -33.8675) || !near(m.Lon, 151.207) {
		t.Errorf("GPS = %v, %v; want -33.8675, 151.207", deref(m.Lat), deref(m.Lon))
	}
	// Orientation 6 is a quarter turn: a 40x30 image displays as 30x40. The
	// header's real size wins over EXIF's PixelXDimension (4000x3000).
	if m.Width == nil || *m.Width != 30 || *m.Height != 40 {
		t.Errorf("dimensions %v x %v; want 30 x 40", derefInt(m.Width), derefInt(m.Height))
	}

	// DateTime pairs with OffsetTime, not OffsetTimeOriginal.
	p = filepath.Join(dir, "b.jpg")
	writeFile(t, p, jpegWithEXIF(t, 40, 30, fullEXIF(0x0132, 0x9010, wall, "+09:00", 10, 20, 1)))
	if m := ReadMetadata(p); m.Source != CaptureFromEXIF || m.Offset != "+09:00" {
		t.Errorf("DateTime + OffsetTime: source %q, offset %q", m.Source, m.Offset)
	}

	// No fix (0,0), and an offset that isn't one: neither is recorded, and
	// the date is read in this machine's zone as before.
	p = filepath.Join(dir, "c.jpg")
	writeFile(t, p, jpegWithEXIF(t, 40, 30, fullEXIF(0x9003, 0x9011, wall, "+2", 0, 0, 1)))
	if m := ReadMetadata(p); m.Lat != nil || m.Offset != "" || m.CapturedAt == nil || int64(*m.CapturedAt) != local.Unix() {
		t.Errorf("no fix, bad offset: lat %v, offset %q, at %v; want none, none, %d", deref(m.Lat), m.Offset, m.CapturedAt, local.Unix())
	}

	// A PNG has no EXIF here: dimensions from its header, date from its path.
	p = filepath.Join(dir, "2019-08-04", "d.png")
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, 12, 7))); err != nil {
		t.Fatal(err)
	}
	writeFile(t, p, buf.Bytes())
	if m := ReadMetadata(p); m.Source != CaptureFromPath || m.Width == nil || *m.Width != 12 || *m.Height != 7 || m.Lat != nil {
		t.Errorf("png: %+v; want path date and 12 x 7", m)
	}

	// A video: a date, nothing else.
	p = filepath.Join(dir, "e.mp4")
	writeFile(t, p, []byte("not parsed"))
	if m := ReadMetadata(p); m.Source != CaptureFromMtime || m.Width != nil || m.Offset != "" {
		t.Errorf("video: %+v; want an mtime date only", m)
	}
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func derefInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// A new file gets its metadata as it's scanned. A file already in the ledger
// from before metadata was read gets it from a later scan that otherwise
// skips it -- once -- and the stored capture time is corrected only where it
// provably came from the same EXIF date; one fix-dates changed is kept, with
// its source left unknown.
func TestScan_RecordsAndBackfillsMetadata(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	wall := "2024:05:01 09:30:00"
	local, _ := time.ParseInLocation("2006:01:02 15:04:05", wall, time.Local)
	zoned, _ := time.Parse("2006:01:02 15:04:05-07:00", wall+"+02:00")

	newFile := filepath.Join(root, "new.jpg")
	writeFile(t, newFile, jpegWithEXIF(t, 40, 30, fullEXIF(0x9003, 0x9011, wall, "+02:00", 41.5, 2.25, 1)))
	old := filepath.Join(root, "old.jpg")
	writeFile(t, old, jpegWithEXIF(t, 40, 30, fullEXIF(0x9003, 0x9011, wall, "+02:00", 41.5, 2.5, 1)))
	fixed := filepath.Join(root, "fixed.jpg")
	writeFile(t, fixed, jpegWithEXIF(t, 40, 30, fullEXIF(0x9003, 0x9011, wall, "+02:00", 41.5, 2.75, 1)))
	if _, err := ScanFolders(db, []string{root}, "manual_scan", 2, false, false, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	shaOf := func(p string) string {
		seen, err := db.GetFileSeen(p)
		if err != nil || seen == nil {
			t.Fatalf("%s not scanned: %v", p, err)
		}
		return seen.SHA256
	}
	row := func(p string) *statedb.MediaRow {
		m, err := db.GetListableMedia(shaOf(p))
		if err != nil || m == nil {
			t.Fatalf("%s: %v", p, err)
		}
		return m
	}

	if m := row(newFile); m.CaptureSource.String != CaptureFromEXIF || m.CaptureOffset.String != "+02:00" ||
		int64(m.CapturedAt) != zoned.Unix() || !m.Lat.Valid || m.Width.Int64 != 40 {
		t.Errorf("new file: %+v; want exif at the camera's offset, GPS and 40 wide", m)
	}

	// Make the other two look like rows from an earlier version: no
	// metadata, and the capture time it would have stored -- the EXIF date
	// read as local -- or, for one, a time fix-dates set.
	fixedAt := float64(time.Date(2023, 1, 2, 12, 0, 0, 0, time.UTC).Unix())
	raw, err := sql.Open("sqlite", statedb.StateDBPath)
	if err != nil {
		t.Fatal(err)
	}
	for sha, at := range map[string]float64{shaOf(old): float64(local.Unix()), shaOf(fixed): fixedAt} {
		if _, err := raw.Exec(`UPDATE uploads SET captured_at = ?, capture_source = NULL, capture_offset = NULL,
			 gps_lat = NULL, gps_lon = NULL, width = NULL, height = NULL, metadata_read_at = NULL WHERE sha256 = ?`, at, sha); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()
	summary, err := ScanFolders(db, []string{root}, "manual_scan", 2, false, false, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if summary.FilesHashed != 0 || summary.MetadataRead != 2 {
		t.Errorf("backfill scan hashed %d and read %d; want 0 and 2", summary.FilesHashed, summary.MetadataRead)
	}
	if m := row(old); m.CaptureSource.String != CaptureFromEXIF || int64(m.CapturedAt) != zoned.Unix() || !near(&m.Lon.Float64, 2.5) {
		t.Errorf("old row: %+v; want exif, corrected to the camera's offset, with GPS", m)
	}
	if m := row(fixed); m.CaptureSource.Valid || m.CapturedAt != fixedAt || m.CaptureOffset.String != "+02:00" {
		t.Errorf("fix-dates row: %+v; want its time kept, source unknown, the offset still recorded", m)
	}

	summary, err = ScanFolders(db, []string{root}, "manual_scan", 2, false, false, nil, nil, nil)
	if err != nil || summary.MetadataRead != 0 {
		t.Errorf("third scan read %d (%v); want 0 -- each row once", summary.MetadataRead, err)
	}
}
