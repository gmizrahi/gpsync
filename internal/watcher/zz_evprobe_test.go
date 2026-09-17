package watcher

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

// TestProbe_RawEventNames prints the raw ev.Name fsnotify delivers, to settle
// whether Windows reports the changed file's own path or the watched root.
// Temporary: this exists to produce evidence in CI, not to assert anything.
func TestProbe_RawEventNames(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "2024", "2024_01")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer fsw.Close()
	for _, d := range []string{root, filepath.Join(root, "2024"), sub} {
		if err := fsw.Add(d); err != nil {
			t.Fatalf("Add(%s): %v", d, err)
		}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.After(3 * time.Second)
		for {
			select {
			case ev, ok := <-fsw.Events:
				if !ok {
					return
				}
				fmt.Printf("PROBE goos=%s op=%-14s name=%q dir=%q\n", runtime.GOOS, ev.Op.String(), ev.Name, filepath.Dir(ev.Name))
			case err := <-fsw.Errors:
				fmt.Printf("PROBE error: %v\n", err)
			case <-deadline:
				return
			}
		}
	}()

	time.Sleep(200 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(sub, "photo.jpg"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	<-done
	fmt.Printf("PROBE expected leaf = %q\n", sub)
}
