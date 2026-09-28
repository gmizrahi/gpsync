package scanner

// SetHashFileForTest replaces the content-hash function the scanning pass uses
// and returns a function that puts the original back. It exists so a test can
// make hashing take a controlled amount of time -- block it, fail it, release
// it -- instead of staging a file large enough that hashing it is slow on real
// hardware. A wall-clock assertion against a real multi-gigabyte file is both
// slow and only probabilistically correct; it depends on the page cache and on
// how loaded the machine is.
//
// Exported only because the tests that need it live in another package
// (internal/engine drives whole scan/upload cycles). internal/... is not
// importable outside this module.
func SetHashFileForTest(fn func(path string) (string, error)) (restore func()) {
	prev := hashFileFn
	hashFileFn = fn
	return func() { hashFileFn = prev }
}
