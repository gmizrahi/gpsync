package dashboard

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/gmizrahi/gpsync/internal/units"
)

// humanCount adds thousands separators (79,192) -- plain %d on a real
// library's file count reads as a wall of digits.
func humanCount(n int) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

// mediaKindLabel maps a stored MIME type ("image/jpeg", "video/mp4") to the
// plain-language label Browse's Type column shows: "Photo" or "Video",
// not the raw MIME string.
func mediaKindLabel(mimeType string) string {
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return "Photo"
	case strings.HasPrefix(mimeType, "video/"):
		return "Video"
	default:
		return "Other"
	}
}

// pairPct formats the two segments of a media-type bar -- uploaded and
// still to do -- so the two figures always add up to exactly 100%.
//
// Only the smaller share is rounded: one decimal ("2.7%"), two below 0.1%
// ("0.05%"), and never below 0.01%, so remaining work is never shown as 0%.
// The larger share is 100 minus that at the same precision, which also means
// it can never read 100% while something is left. A bar with only one
// segment shows "100%" on it.
//
// The shares are of uploaded + outstanding only. Files set aside on purpose
// (ignored originals) belong to neither segment; they still count in the
// category total beside the bar.
func pairPct(done, pending int64) (doneStr, pendingStr string) {
	switch {
	case done <= 0 && pending <= 0:
		return "0%", "0%"
	case pending <= 0:
		return "100%", "0%"
	case done <= 0:
		return "0%", "100%"
	}
	small, smallIsPending := pending, true
	if done < pending {
		small, smallIsPending = done, false
	}
	share := float64(small) / float64(done+pending) * 100
	decimals, units := 1, 10.0
	if share < 0.1 {
		decimals, units = 2, 100.0
	}
	smallUnits := int64(math.Round(share * units))
	if smallUnits < 1 {
		smallUnits = 1
	}
	bigUnits := int64(100*units) - smallUnits
	format := func(n int64) string {
		return strconv.FormatFloat(float64(n)/units, 'f', decimals, 64) + "%"
	}
	if smallIsPending {
		return format(bigUnits), format(smallUnits)
	}
	return format(smallUnits), format(bigUnits)
}

// humanBytes is units.Bytes: KB/MB/GB/TB labels, shared with the CLI so the
// two can never disagree. (It used to be a duplicate, on the mistaken
// grounds that both were package main -- this package never was.)
func humanBytes(n int64) string {
	return units.Bytes(n)
}
