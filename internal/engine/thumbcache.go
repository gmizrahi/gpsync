package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/fsperm"
	"github.com/gmizrahi/gpsync/internal/pathx"
	"github.com/gmizrahi/gpsync/internal/preprocess"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// ThumbSizes are the only thumbnail sizes the media API makes: a request
// snaps to the next one up (SnapThumbSize), which bounds both the CPU spent
// and how many cached copies one file can have.
var ThumbSizes = []int{256, 512, 1024}

// SnapThumbSize maps a requested size to the smallest of ThumbSizes at least
// as large, or the largest; 0 (not given) is the smallest.
func SnapThumbSize(px int) int {
	for _, s := range ThumbSizes {
		if px <= s {
			return s
		}
	}
	return ThumbSizes[len(ThumbSizes)-1]
}

// ThumbCacheMaxBytes bounds the thumbnail cache on disk. At about 15 KB for
// a 256 px thumbnail and 150 KB for 1024 px, it holds thousands of either.
const ThumbCacheMaxBytes = 1 << 30

// ThumbCache keeps generated thumbnails at <dir>/<size>/<sha256>.jpg.
// Keyed by content hash, an entry never goes stale -- the same hash is the
// same picture -- so it is only ever removed to stay under its size bound,
// least recently used first.
//
// The folder is one the user may choose, even inside their library, so the
// cache only ever reads, counts or removes files it names itself: anything
// else in that folder is left alone.
type ThumbCache struct {
	maxBytes int64
	// gen bounds how many thumbnails are decoded at once: a client asking
	// for a whole page of them must not take every core.
	gen chan struct{}

	mu       sync.Mutex
	dir      string
	total    int64 // bytes on disk; -1 until first counted
	inflight map[string]chan struct{}
}

// NewThumbCache returns a cache in dir, bounded to maxBytes. Nothing is
// created on disk until a thumbnail is. UseDir moves it.
func NewThumbCache(dir string, maxBytes int64) *ThumbCache {
	return &ThumbCache{
		dir:      dir,
		maxBytes: maxBytes,
		gen:      make(chan struct{}, max(1, runtime.NumCPU()/2)),
		total:    -1,
		inflight: map[string]chan struct{}{},
	}
}

// UseDir points the cache at dir, the thumbnail_dir setting as it is now: a
// Settings change applies to the next thumbnail, with no restart. The old
// folder is left as it is.
func (c *ThumbCache) UseDir(dir string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if dir != c.dir {
		c.dir, c.total = dir, -1
	}
}

var thumbFileName = regexp.MustCompile(`^[0-9a-f]{64}\.jpg$`)

// Thumbnail returns the JPEG thumbnail of the file at path, whose content
// hash is sha, at a size from ThumbSizes: from the cache, or generated and
// cached; fromCache says which. ErrNoThumbnail if the format has none;
// ErrMediaUnavailable if the file can't be decoded.
func (c *ThumbCache) Thumbnail(sha, path string, size int) (data []byte, fromCache bool, err error) {
	if !preprocess.IsResizable(path) {
		return nil, false, ErrNoThumbnail
	}
	c.mu.Lock()
	dir := c.dir
	c.mu.Unlock()
	file := filepath.Join(dir, strconv.Itoa(size), sha+".jpg")
	for {
		if data, err := os.ReadFile(file); err == nil {
			// The modification time is the recency eviction goes by.
			now := time.Now()
			_ = os.Chtimes(file, now, now)
			return data, true, nil
		}
		// One generation per entry at a time: two requests for the same
		// thumbnail would otherwise stage the same file at once.
		c.mu.Lock()
		wait, busy := c.inflight[file]
		if !busy {
			c.inflight[file] = make(chan struct{})
		}
		c.mu.Unlock()
		if !busy {
			break
		}
		<-wait // then read what it wrote, or generate it if it failed
	}
	defer func() {
		c.mu.Lock()
		close(c.inflight[file])
		delete(c.inflight, file)
		c.mu.Unlock()
	}()

	c.gen <- struct{}{}
	data, _, ok := GenerateThumbnail(path, size)
	<-c.gen
	if !ok {
		return nil, false, ErrMediaUnavailable
	}
	// A cache that can't be written still serves the thumbnail it made.
	if err := ensureThumbDir(dir, filepath.Dir(file)); err == nil {
		if err := fsperm.WriteFileOwnerOnlyWhereSupported(file, data); err == nil {
			c.added(dir, int64(len(data)))
		}
	}
	return data, false, nil
}

// ensureThumbDir creates the cache folder and the size folder under it. A
// cache folder gpsync creates with a dot-name, like .thumbnails, is also
// marked hidden on Windows, where the dot alone doesn't hide it.
func ensureThumbDir(root, sub string) error {
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return err
		}
		if strings.HasPrefix(filepath.Base(root), ".") {
			_ = fsperm.MarkHidden(root)
		}
	}
	return os.MkdirAll(sub, 0o700)
}

// added accounts for a new entry and, past the bound, removes the least
// recently used ones until the cache is at 90% of it -- some headroom, so
// it isn't swept again on the very next write.
func (c *ThumbCache) added(dir string, n int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if dir != c.dir {
		return // moved meanwhile: the new folder is counted when first used
	}
	if c.total < 0 {
		c.total = 0
		for _, e := range c.entries() {
			c.total += e.size
		}
	} else {
		c.total += n
	}
	if c.total <= c.maxBytes {
		return
	}
	entries := c.entries()
	sort.Slice(entries, func(i, j int) bool { return entries[i].used.Before(entries[j].used) })
	c.total = 0
	for _, e := range entries {
		c.total += e.size
	}
	for _, e := range entries {
		if c.total <= c.maxBytes*9/10 {
			break
		}
		if err := os.Remove(e.path); err == nil || errors.Is(err, fs.ErrNotExist) {
			c.total -= e.size
		}
	}
}

type thumbEntry struct {
	path string
	size int64
	used time.Time
}

func (c *ThumbCache) entries() []thumbEntry { return thumbEntries(c.dir) }

// thumbEntries lists a cache folder's own files: <size>/<sha256>.jpg for
// the sizes it makes, and nothing else in the folder.
func thumbEntries(dir string) []thumbEntry {
	var out []thumbEntry
	for _, size := range ThumbSizes {
		sub := filepath.Join(dir, strconv.Itoa(size))
		list, err := os.ReadDir(sub)
		if err != nil {
			continue
		}
		for _, d := range list {
			if !d.Type().IsRegular() || !thumbFileName.MatchString(d.Name()) {
				continue
			}
			if info, err := d.Info(); err == nil {
				out = append(out, thumbEntry{filepath.Join(sub, d.Name()), info.Size(), info.ModTime()})
			}
		}
	}
	return out
}

// ThumbnailDir is the folder the thumbnail cache uses: the thumbnail_dir
// setting, or thumbs in gpsync's state folder. A set one must be a full
// path, and must not be a source folder or contain one -- scans skip it, so
// either would hide photos from them. Inside a source folder is fine.
func ThumbnailDir(cfg config.Config) (string, error) {
	dir := strings.TrimSpace(cfg.ThumbnailDir)
	if dir == "" {
		return filepath.Join(statedb.StateDir, "thumbs"), nil
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("the thumbnail folder must be a full path, such as D:\\Pictures\\.thumbnails (got %q)", dir)
	}
	dir = filepath.Clean(dir)
	for _, src := range cfg.SourceFolders {
		if pathx.Within(src, dir) {
			return "", fmt.Errorf("the thumbnail folder %s would hide the source folder %s from scans: choose a folder of its own (inside a source folder is fine)", dir, src)
		}
	}
	return dir, nil
}
