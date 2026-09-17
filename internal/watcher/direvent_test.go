package watcher

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fsnotify/fsnotify"
)

// TestWatcher_DirectoryWriteDoesNotDebounceItsParent pins the fix for a real
// Windows defect found by probing the raw fsnotify events on a CI runner.
//
// Windows emits a WRITE on the PARENT DIRECTORY alongside a file's own
// events; Linux does not. handleEvent used to filter directories only on
// Create, so that WRITE fell through to resetTimer(filepath.Dir(ev.Name))
// and debounced the directory's parent -- for a top-level folder, the entire
// source root. It arrived before the file's own event, so Ready() reported
// the root and gpsync rescanned everything under it over a single edit.
//
// Exercised by synthesising the event rather than by writing to a directory,
// because the extra WRITE only occurs on Windows: driving handleEvent
// directly makes the rule hold on every platform, which is the point.
func TestWatcher_DirectoryWriteDoesNotDebounceItsParent(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "2024", "2024_01")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	w, err := New([]string{root}, testDebounce)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// The Windows-only event: a WRITE naming a DIRECTORY.
	w.handleEvent(fsnotifyWrite(filepath.Join(root, "2024")))
	assertNoReady(t, w, testDebounce*3)

	// A real file change in the same tree must still fire, for the leaf.
	mustWriteFile(t, filepath.Join(sub, "photo.jpg"), []byte("x"))
	if got := waitReady(t, w); got != sub {
		t.Errorf("Ready() folder = %q, want %q", got, sub)
	}
}

// fsnotifyWrite builds the event shape Windows delivers for a directory.
func fsnotifyWrite(name string) fsnotify.Event {
	return fsnotify.Event{Name: name, Op: fsnotify.Write}
}
