package preprocess

import (
	"bytes"
	"encoding/binary"
	"os"
)

// JPEG markers. A JPEG is a sequence of segments: 0xFF, a marker byte, then
// (for most markers) a two-byte big-endian length covering the length field
// itself.
const (
	markerPrefix = 0xFF
	markerSOI    = 0xD8 // start of image
	markerAPP1   = 0xE1 // where EXIF lives
	markerSOS    = 0xDA // start of scan: entropy-coded data follows, stop parsing
)

var exifIdentifier = []byte("Exif\x00\x00")

// readEXIFSegment returns the raw APP1/EXIF segment of a JPEG, including its
// 0xFFE1 marker and length, or nil when the file has none.
//
// Returned verbatim rather than re-encoded: every tag the source carried
// survives, including ones this package has no opinion about. Re-serialising
// EXIF would mean understanding it, and getting that wrong silently corrupts
// metadata.
func readEXIFSegment(path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil || len(data) < 4 {
		return nil
	}
	if data[0] != markerPrefix || data[1] != markerSOI {
		return nil // not a JPEG
	}

	for i := 2; i+4 <= len(data); {
		if data[i] != markerPrefix {
			return nil // out of sync; refuse to guess
		}
		marker := data[i+1]
		if marker == markerSOS {
			return nil // image data from here on
		}
		segLen := int(binary.BigEndian.Uint16(data[i+2 : i+4]))
		if segLen < 2 || i+2+segLen > len(data) {
			return nil // truncated
		}
		if marker == markerAPP1 {
			payload := data[i+4 : i+2+segLen]
			if bytes.HasPrefix(payload, exifIdentifier) {
				return data[i : i+2+segLen]
			}
		}
		i += 2 + segLen
	}
	return nil
}

// normaliseOrientation rewrites the Orientation tag (0x0112) to 1 in place.
//
// This is not optional. imaging.Open is called with AutoOrientation(true),
// which rotates the PIXELS to match the tag. Copying the original tag onto
// already-rotated pixels would make every viewer rotate them a second time,
// so a portrait photo would arrive in Google Photos on its side.
//
// Returns false when the segment cannot be parsed confidently, in which case
// the caller must drop the EXIF rather than attach something it does not
// understand.
func normaliseOrientation(seg []byte) bool {
	tiff := seg[4+len(exifIdentifier):]
	if len(tiff) < 8 {
		return false
	}
	var bo binary.ByteOrder
	switch {
	case tiff[0] == 'I' && tiff[1] == 'I':
		bo = binary.LittleEndian
	case tiff[0] == 'M' && tiff[1] == 'M':
		bo = binary.BigEndian
	default:
		return false
	}

	ifdOffset := int(bo.Uint32(tiff[4:8]))
	if ifdOffset < 8 || ifdOffset+2 > len(tiff) {
		return false
	}
	count := int(bo.Uint16(tiff[ifdOffset : ifdOffset+2]))
	for e := range count {
		// Each IFD entry is 12 bytes: tag, type, count, value/offset.
		off := ifdOffset + 2 + e*12
		if off+12 > len(tiff) {
			return false
		}
		if bo.Uint16(tiff[off:off+2]) == 0x0112 {
			// SHORT value, stored inline in the first 2 bytes of the field.
			bo.PutUint16(tiff[off+8:off+10], 1)
			return true
		}
	}
	// No Orientation tag at all: nothing to correct, nothing to double-apply.
	return true
}

// spliceEXIF inserts seg into a JPEG immediately after SOI, replacing any
// APP1 the encoder happened to write. Returns false if dst is not a JPEG.
func spliceEXIF(dstPath string, seg []byte) bool {
	data, err := os.ReadFile(dstPath)
	if err != nil || len(data) < 2 || data[0] != markerPrefix || data[1] != markerSOI {
		return false
	}
	out := make([]byte, 0, len(data)+len(seg))
	out = append(out, data[:2]...)
	out = append(out, seg...)
	out = append(out, data[2:]...)
	// 0600: a downscaled copy of someone's photo is still their photo, and
	// it sits in a shared temp directory until the upload finishes.
	return os.WriteFile(dstPath, out, 0o600) == nil
}

// carryEXIF copies srcPath's EXIF onto dstPath, with Orientation normalised.
// Best-effort by design: a source without EXIF, or one this cannot parse
// confidently, leaves dstPath exactly as the encoder wrote it.
func carryEXIF(srcPath, dstPath string) {
	seg := readEXIFSegment(srcPath)
	if seg == nil {
		return
	}
	// Copy before mutating: readEXIFSegment returns a slice into the file
	// buffer, and normaliseOrientation writes into it.
	seg = append([]byte(nil), seg...)
	if !normaliseOrientation(seg) {
		return
	}
	spliceEXIF(dstPath, seg)
}
