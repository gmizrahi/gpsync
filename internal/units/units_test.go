package units

import (
	"strings"
	"testing"
)

// TestBytes pins both halves of the request: KB/MB/GB/TB labels, and the
// 1024-based magnitudes Windows Explorer shows for the same files, so no
// displayed figure changes -- only its label.
func TestBytes(t *testing.T) {
	const kb, mb, gb, tb = int64(1) << 10, int64(1) << 20, int64(1) << 30, int64(1) << 40
	for _, c := range []struct {
		n    int64
		want string
	}{
		{0, "0B"},
		{500, "500B"},
		{1023, "1023B"},
		{kb, "1.0KB"},
		{1536, "1.5KB"},
		{3 * kb, "3.0KB"},
		{mb, "1.0MB"},
		{gb, "1.0GB"},
		{286*gb + gb/5, "286.2GB"},
		{tb, "1.0TB"},
		{int64(1.5 * float64(tb)), "1.5TB"},
	} {
		if got := Bytes(c.n); got != c.want {
			t.Errorf("Bytes(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

// TestBytes_NoBinaryPrefixes is the regression guard for the complaint
// itself: no magnitude may ever render with an "iB" label again.
func TestBytes_NoBinaryPrefixes(t *testing.T) {
	for n := int64(1); n > 0 && n < int64(1)<<62; n *= 7 {
		if got := Bytes(n); strings.Contains(got, "iB") {
			t.Fatalf("Bytes(%d) = %q -- binary-prefix labels (KiB/MiB/GiB/TiB) were replaced by KB/MB/GB/TB on request", n, got)
		}
	}
}
