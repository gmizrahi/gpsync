// Command demo-ledger writes a synthetic ledger for documentation
// screenshots, so the images in README.md and docs/ never come from anyone's
// real photo library.
//
// Every folder name, file name and hash here is invented. The data is
// deterministic: the same invocation always produces the same ledger, so a
// screenshot can be regenerated later and diffed against the one in the repo.
//
// Not built by the Makefile and never deployed.
//
//	GPSYNC_STATE_DIR=/tmp/gpsync-demo go run ./cmd/demo-ledger
//	GPSYNC_STATE_DIR=/tmp/gpsync-demo go run ./cmd/dashboard-preview -addr 127.0.0.1:18080
//
// It refuses to run without GPSYNC_STATE_DIR, for the same reason
// dashboard-preview does: the default state directory is the real one.
//
// Paths are Windows paths, because that is gpsync's primary platform. The
// dashboard renders them correctly on any host: it splits paths for display
// with pathx.DisplayDir/DisplayName, which recognise both separators.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"math/rand"
	"os"
	"time"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// Folder names are deliberately generic: a screenshot should show the shape
// of a library, not anyone's holidays.
var folders = []struct {
	year int
	name string
}{
	{2019, "2019_04 City Break"},
	{2019, "2019_08 Summer Trip"},
	{2020, "2020_01 Winter Walk"},
	{2020, "2020_07 Lake Weekend"},
	{2021, "2021_03 Garden"},
	{2021, "2021_06 Coast Road"},
	{2021, "2021_11 Autumn Woods"},
	// 2022 carries the folders cmd/dashboard-preview's stub controller shows
	// as in flight and recently finished, so a Status screenshot never names
	// a folder the Browse table below it does not have.
	{2022, "2022_03 Race Weekend"},
	{2022, "2022_07 Summer Trip"},
	{2022, "2022_07 Theme Park"},
	{2022, "2022_09 Birthday"},
	{2022, "2022_09 Get-together"},
	{2022, "2022_10 Boat Trip"},
	{2022, "2022_10 City Break"},
	{2022, "2022_10 Mountains"},
	{2022, "2022_10 Museum Day"},
	{2022, "2022_10 Sightseeing"},
	{2022, "2022_10 Weekend - Hotel"},
	{2023, "2023_01 New Year"},
	{2023, "2023_06 Mountains"},
	{2023, "2023_10 Old Town"},
	{2024, "2024_03 Spring"},
	{2024, "2024_07 Beach Days"},
	{2024, "2024_12 Snow"},
	{2025, "2025_05 Road Trip"},
	{2025, "2025_09 Theme Park"},
	{2026, "2026_02 Rainy Weekend"},
	{2026, "2026_06 Long Weekend"},
	{2026, "2026_09 Late Summer"},
}

const root = `C:\Photos`

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func main() {
	if os.Getenv("GPSYNC_STATE_DIR") == "" {
		log.Fatal("GPSYNC_STATE_DIR must point at a scratch directory -- refusing to write to the real state directory")
	}
	db, err := statedb.Open()
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// Fixed seed: the same ledger every time, so a regenerated screenshot
	// differs only where the code changed.
	rng := rand.New(rand.NewSource(20260917)) // #nosec G404 -- fixture data, not security

	var uploaded, pending, retryable, permanent, review int
	for _, f := range folders {
		perFolder := 40 + rng.Intn(90)
		for i := range perFolder {
			video := rng.Intn(9) == 0
			ext, mime := ".jpg", "image/jpeg"
			size := int64(2<<20) + rng.Int63n(6<<20)
			if video {
				ext, mime = ".mp4", "video/mp4"
				size = int64(180<<20) + rng.Int63n(1200<<20)
			}
			prefix := "IMG"
			if video {
				prefix = "VID"
			}
			name := fmt.Sprintf("%s%02d%02d_%02d%02d%02d%s",
				prefix, f.year%100, 1+rng.Intn(12), rng.Intn(24), rng.Intn(60), rng.Intn(60), ext)
			p := fmt.Sprintf(`%s\%d\%s\%s`, root, f.year, f.name, name)
			sha := hashOf(p)

			captured := float64(time.Date(f.year, time.Month(1+rng.Intn(12)), 1+rng.Intn(28),
				rng.Intn(24), rng.Intn(60), 0, 0, time.UTC).Unix())

			if err := db.EnsurePending(sha, size, mime, p, &captured); err != nil {
				log.Fatal(err)
			}
			if err := db.UpsertFileSeen(p, sha, captured, size); err != nil {
				log.Fatal(err)
			}

			// Recent folders still have outstanding work; older ones are done.
			// That is what a library mid-sync actually looks like.
			outstanding := f.year >= 2026 && i%3 == 0
			switch {
			case outstanding && i%9 == 0:
				if err := db.MarkFailed(sha, false, "THROTTLE", "Quota exceeded for quota 'concurrent write request'"); err != nil {
					log.Fatal(err)
				}
				retryable++
			case outstanding:
				pending++ // left as pending
			case i%47 == 0:
				if err := db.MarkFailed(sha, true, "UNSUPPORTED_EXTENSION", "'.bmp' is not supported by Google Photos"); err != nil {
					log.Fatal(err)
				}
				permanent++
			default:
				if err := db.MarkUploaded(sha, "media-"+sha[:16], "original"); err != nil {
					log.Fatal(err)
				}
				uploaded++
			}
		}
	}

	// A few originals-folder items awaiting review, and one duplicate group,
	// so those pages are not empty in a screenshot.
	for i := range 3 {
		p := fmt.Sprintf(`%s\2024\2024_07 Beach Days\originals\IMG_%04d.jpg`, root, 1200+i)
		sha := hashOf(p)
		captured := float64(time.Date(2024, 7, 12, 10, 0, 0, 0, time.UTC).Unix())
		if err := db.EnsureNeedsReview(sha, 4<<20, "image/jpeg", p, &captured); err != nil {
			log.Fatal(err)
		}
		if err := db.UpsertFileSeen(p, sha, captured, 4<<20); err != nil {
			log.Fatal(err)
		}
		review++
	}
	for i := range 4 {
		name := fmt.Sprintf("IMG_%04d.jpg", 3300+i)
		orig := fmt.Sprintf(`%s\2023\2023_06 Mountains\%s`, root, name)
		dup := fmt.Sprintf(`%s\2023\2023_06 Mountains\copies\%s`, root, name)
		sha := hashOf(orig)
		captured := float64(time.Date(2023, 6, 4, 9, 30, 0, 0, time.UTC).Unix())
		size := int64(5 << 20)
		if err := db.EnsurePending(sha, size, "image/jpeg", orig, &captured); err != nil {
			log.Fatal(err)
		}
		if err := db.MarkUploaded(sha, "media-"+sha[:16], "original"); err != nil {
			log.Fatal(err)
		}
		if err := db.UpsertFileSeen(orig, sha, captured, size); err != nil {
			log.Fatal(err)
		}
		if err := db.UpsertFileSeen(dup, sha, captured, size); err != nil {
			log.Fatal(err)
		}
	}

	// Throttle history, so the Statistics page's recovery card has data.
	schedule := []float64{300, 600, 900, 1800, 3600}
	for i := range 14 {
		rung := i % len(schedule)
		if err := db.RecordThrottleEvent(rung, schedule[rung],
			"Quota exceeded for quota 'concurrent write request' of service 'photoslibrary.googleapis.com'"); err != nil {
			log.Fatal(err)
		}
	}

	cfg := config.Defaults()
	cfg.SourceFolders = []string{root}
	cfg.Theme = config.ThemeDark
	cfg.BackupDir = `C:\GPSyncBackups`
	cfg.TrashDir = `C:\GPSyncTrash`
	cfg.DupesDir = `C:\GPSyncDupes`
	if err := config.Save(cfg); err != nil {
		log.Fatal(err)
	}

	fmt.Printf("demo ledger written to %s\n", statedb.StateDBPath)
	fmt.Printf("  uploaded %d  pending %d  retryable %d  permanent %d  needs review %d\n",
		uploaded, pending, retryable, permanent, review)
}
