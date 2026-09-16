// Package hashing computes the streaming SHA-256 content hash that is the
// identity key for the upload ledger — the same file content under any
// filename or path is recognized as already uploaded.
package hashing

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
)

// SHA256File returns the hex SHA-256 digest of a file's contents, reading in
// chunks so large video files don't need to fit in memory.
func SHA256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
