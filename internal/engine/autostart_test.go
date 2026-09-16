package engine

import "testing"

func TestAutostartCommand_QuotesThePath(t *testing.T) {
	cases := []struct{ path, want string }{
		{`C:\GPSync\gpsync-tray.exe`, `"C:\GPSync\gpsync-tray.exe"`},
		{`C:\Program Files\GPSync\gpsync-tray.exe`, `"C:\Program Files\GPSync\gpsync-tray.exe"`},
	}
	for _, c := range cases {
		if got := AutostartCommand(c.path); got != c.want {
			t.Errorf("AutostartCommand(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}
