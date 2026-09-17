// Package watcher turns fsnotify's flat, non-recursive directory watching
// into "tell me which leaf folder went quiet after a change" for `gpsync
// watch`. fsnotify only ever watches directories you explicitly Add() to
// it (its own doc comment is explicit: "Recursive watching is not
// currently enabled through fsnotify's public API") -- this package walks
// a tree once at startup, adds a watch to every directory in it, and keeps
// adding watches for new directories as they're created, so a whole new
// year/month folder shows up without restarting gpsync.
package watcher

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/gmizrahi/gpsync/internal/scanner"
)

// Watcher watches one or more directory trees and reports, on Ready, each
// leaf folder that went quiet for `debounce` after its last observed
// change -- a burst of file-copy activity in one folder collapses to a
// single signal once it settles, rather than one per file.
type Watcher struct {
	fsw      *fsnotify.Watcher
	debounce time.Duration
	ready    chan string
	errs     chan error

	mu     sync.Mutex
	timers map[string]*time.Timer

	closeOnce sync.Once
	done      chan struct{}
}

// New starts watching every directory under each of roots, recursively,
// and returns once the initial tree walk has added a watch to all of
// them. Ignored directories (scanner.IsIgnoredDirName -- e.g. Picasa's
// .picasaoriginals) are skipped entirely, the same exclusion `gpsync scan`
// already applies, so the watcher doesn't spend a watch handle (a limited
// resource, especially on Windows) on a tree gpsync would never scan anyway.
//
// A failure to add one particular directory (permissions, a symlink loop,
// it vanishing mid-walk) does not abort the whole call -- it's reported on
// Errors() and that one directory is simply not watched. New() only
// returns an error if fsnotify itself can't start, or every single root
// failed outright.
func New(roots []string, debounce time.Duration) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	w := &Watcher{
		fsw:      fsw,
		debounce: debounce,
		ready:    make(chan string),
		errs:     make(chan error, 16),
		timers:   map[string]*time.Timer{},
		done:     make(chan struct{}),
	}

	var lastErr error
	watchedAny := false
	for _, root := range roots {
		if err := w.addTree(root); err != nil {
			lastErr = err
			w.reportErr(err)
			continue
		}
		watchedAny = true
	}
	if !watchedAny && len(roots) > 0 {
		fsw.Close()
		if lastErr == nil {
			lastErr = fmt.Errorf("no root could be watched")
		}
		return nil, lastErr
	}

	go w.loop()
	return w, nil
}

// Ready emits a leaf folder path once `debounce` has passed since the last
// change observed in it. Unbuffered by design: a folder's own cycle
// naturally serializes behind whatever the caller is currently doing with
// the previous one, which is exactly the "process one folder at a time"
// behavior the rest of gpsync already has (see cmd/gpsync's syncFolders/
// uploadFolders loops) -- it does not block watching for OTHER folders'
// changes or picking up newly-created subdirectories, since those run on
// the watcher's own separate internal goroutine.
func (w *Watcher) Ready() <-chan string { return w.ready }

// Errors reports non-fatal problems: a directory that couldn't be added
// (mid-walk or dynamically, on a later Create), or an error fsnotify's own
// backend reported. Never blocks a caller who isn't reading it -- buffered,
// and a full buffer just drops the oldest-unread error rather than stalling
// the watcher.
func (w *Watcher) Errors() <-chan error { return w.errs }

// Close stops the watcher: no further sends on Ready, no more directories
// added. Safe to call more than once. Callers must stop reading Ready()
// once Close returns -- the channel is deliberately never closed itself,
// since a debounce timer's callback goroutine can be mid-send at exactly
// the moment Close runs, and closing a channel out from under a concurrent
// send is a panic, not a graceful shutdown.
func (w *Watcher) Close() error {
	w.closeOnce.Do(func() { close(w.done) })
	w.mu.Lock()
	for _, t := range w.timers {
		t.Stop()
	}
	w.mu.Unlock()
	return w.fsw.Close()
}

// addTree walks root and every directory beneath it (skipping ignored
// ones) and adds a watch to each. Used both for the initial roots and,
// dynamically, for a directory the watcher sees created later -- e.g. an
// entire populated folder copied or moved in at once (xcopy, drag-and-drop)
// needs every one of ITS subdirectories watched too, not just itself.
func (w *Watcher) addTree(root string) error {
	// The root itself failing (doesn't exist, not a directory, no
	// permission) is a real per-root failure -- distinct from a failure
	// found partway through the walk, which stays best-effort below.
	if info, err := os.Stat(root); err != nil {
		return err
	} else if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", root)
	}

	rootBase := filepath.Base(root)
	first := true
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Best-effort: one unreadable subdirectory shouldn't take down
			// watching everything else under this root.
			w.reportErr(err)
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		// Never skip the root itself, even if its own name happens to
		// collide with an ignore pattern -- the caller asked for it
		// explicitly. Only entries found DURING the walk are gated,
		// mirroring scanner.ExpandFolders' own walk.
		if !(first && d.Name() == rootBase) && scanner.IsIgnoredDirName(d.Name()) {
			return filepath.SkipDir
		}
		first = false
		if err := w.fsw.Add(path); err != nil {
			w.reportErr(err)
			return nil
		}
		return nil
	})
}

func (w *Watcher) reportErr(err error) {
	select {
	case w.errs <- err:
	default:
		// Buffer's full and nobody's draining it fast enough -- drop
		// rather than block the watch loop over a secondary error channel.
	}
}

func (w *Watcher) loop() {
	for {
		select {
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			w.handleEvent(ev)
		case err, ok := <-w.fsw.Errors:
			if !ok {
				continue
			}
			w.reportErr(err)
		case <-w.done:
			return
		}
	}
}

func (w *Watcher) handleEvent(ev fsnotify.Event) {
	base := filepath.Base(ev.Name)
	// A bare Chmod (attribute-only change, no content change) is exactly
	// the kind of noise fsnotify's own docs warn about -- anti-virus/backup
	// software touching metadata constantly. Only reacted to when it rides
	// along with a real content-affecting op.
	if ev.Op == fsnotify.Chmod {
		return
	}
	if scanner.IsIgnoredFileName(base) {
		return
	}

	// A DIRECTORY event is never a file change, whatever the op. Windows
	// emits a WRITE on the parent directory alongside the file's own
	// events, which Linux does not; without this check that WRITE fell
	// through to resetTimer(filepath.Dir(dir)) and debounced the dir's
	// PARENT -- for a top-level folder, the whole source root. It arrives
	// first, so Ready() reported the root before the real file event could
	// report the leaf, and gpsync rescanned the entire root over one edit.
	if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
		// A newly created directory still needs watching; whatever lands
		// inside it then generates its own events (and now that it is
		// watched, gpsync sees them).
		if ev.Has(fsnotify.Create) && !scanner.IsIgnoredDirName(base) {
			if err := w.addTree(ev.Name); err != nil {
				w.reportErr(err)
			}
		}
		return
	}

	w.resetTimer(filepath.Dir(ev.Name))
}

// resetTimer (re)starts the debounce window for folder, so a burst of
// activity collapses into one Ready signal once it settles rather than
// firing on every single event.
func (w *Watcher) resetTimer(folder string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t, ok := w.timers[folder]; ok {
		t.Reset(w.debounce)
		return
	}
	w.timers[folder] = time.AfterFunc(w.debounce, func() {
		w.mu.Lock()
		delete(w.timers, folder)
		w.mu.Unlock()
		select {
		case w.ready <- folder:
		case <-w.done:
		}
	})
}
