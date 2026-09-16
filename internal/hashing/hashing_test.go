package hashing

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func expected(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func TestSHA256File_KnownVector(t *testing.T) {
	dir := t.TempDir()
	data := []byte("hello world")
	p := writeTemp(t, dir, "hello.txt", data)

	got, err := SHA256File(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != expected(data) {
		t.Errorf("got %s, want %s", got, expected(data))
	}
}

func TestSHA256File_Empty(t *testing.T) {
	dir := t.TempDir()
	p := writeTemp(t, dir, "empty.bin", []byte{})

	got, err := SHA256File(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != expected([]byte{}) {
		t.Errorf("got %s, want %s", got, expected([]byte{}))
	}
}

func TestSHA256File_LargeStreamsCorrectly(t *testing.T) {
	dir := t.TempDir()
	// Bigger than a typical 1MiB read buffer, to exercise multiple io.Copy reads.
	data := make([]byte, 1024*1024+12345)
	for i := range data {
		data[i] = byte(i % 256)
	}
	p := writeTemp(t, dir, "big.bin", data)

	got, err := SHA256File(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != expected(data) {
		t.Errorf("got %s, want %s", got, expected(data))
	}
}

func TestSHA256File_IdenticalContentDifferentNamesSameHash(t *testing.T) {
	dir := t.TempDir()
	data := []byte("same content, different filename")
	p1 := writeTemp(t, dir, "a.jpg", data)
	p2 := writeTemp(t, dir, "b_copy.jpg", data)

	h1, err := SHA256File(p1)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := SHA256File(p2)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Errorf("expected identical content to hash the same, got %s vs %s", h1, h2)
	}
}

func TestSHA256File_MissingFileErrors(t *testing.T) {
	_, err := SHA256File(filepath.Join(t.TempDir(), "does-not-exist.jpg"))
	if err == nil {
		t.Error("expected an error for a missing file")
	}
}
