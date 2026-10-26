package engine

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/gmizrahi/gpsync/internal/fsperm"
	"github.com/gmizrahi/gpsync/internal/preprocess"
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
type ThumbCache struct {
	dir      string
	maxBytes int64
	// gen bounds how many thumbnails are decoded at once: a client asking
	// for a whole page of them must not take every core.
	gen chan struct{}

	mu       sync.Mutex
	total    int64 // bytes on disk; -1 until first counted
	inflight map[string]chan struct{}
}

// NewThumbCache returns a cache in dir, bounded to maxBytes. Nothing is
// created on disk until a thumbnail is.
func NewThumbCache(dir string, maxBytes int64) *ThumbCache {
	return &ThumbCache{
		dir:      dir,
		maxBytes: maxBytes,
		gen:      make(chan struct{}, max(1, runtime.NumCPU()/2)),
		total:    -1,
		inflight: map[string]chan struct{}{},
	}
}

// Thumbnail returns the JPEG thumbnail of the file at path, whose content
// hash is sha, at a size from ThumbSizes: from the cache, or generated and
// cached. ErrNoThumbnail if the format has none; ErrMediaUnavailable if the
// file can't be decoded.
func (c *ThumbCache) Thumbnail(sha, path string, size int) ([]byte, error) {
	if !preprocess.IsResizable(path) {
		return nil, ErrNoThumbnail
	}
	file := filepath.Join(c.dir, strconv.Itoa(size), sha+".jpg")
	for {
		if data, err := os.ReadFile(file); err == nil {
			// The modification time is the recency eviction goes by.
			now := time.Now()
			_ = os.Chtimes(file, now, now)
			return data, nil
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
		return nil, ErrMediaUnavailable
	}
	// A cache that can't be written still serves the thumbnail it made.
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err == nil {
		if err := fsperm.WriteFileOwnerOnly(file, data); err == nil {
			c.added(int64(len(data)))
		}
	}
	return data, nil
}

// added accounts for a new entry and, past the bound, removes the least
// recently used ones until the cache is at 90% of it -- some headroom, so
// it isn't swept again on the very next write.
func (c *ThumbCache) added(n int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
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

func (c *ThumbCache) entries() []thumbEntry {
	var out []thumbEntry
	_ = filepath.WalkDir(c.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != ".jpg" {
			return nil
		}
		if info, err := d.Info(); err == nil {
			out = append(out, thumbEntry{p, info.Size(), info.ModTime()})
		}
		return nil
	})
	return out
}
