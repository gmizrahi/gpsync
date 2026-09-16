package dashboard

import (
	"net"
	"testing"
	"time"
)

func occupyPort(t *testing.T) (net.Listener, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln, ln.Addr().(*net.TCPAddr).Port
}

// TestListenPreferred_ConfiguredPortUnavailable_NeverPersists is the
// reported bug: "i force a port and you keep randomizing it". A configured
// port that could not be bound (on the real machine, because Hyper-V had
// reserved the block it sat in) used to be replaced by an OS-assigned
// port, and that port was saved over the user's choice in config.toml.
//
// The dashboard still has to come up, so a temporary port is fine. Writing
// it back is not.
func TestListenPreferred_ConfiguredPortUnavailable_NeverPersists(t *testing.T) {
	old := listenRetryInterval
	listenRetryInterval = 20 * time.Millisecond
	defer func() { listenRetryInterval = old }()

	blocker, taken := occupyPort(t)
	defer blocker.Close()

	res, err := ListenPreferred("127.0.0.1", taken, 150*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Listener.Close()

	if res.Port == taken {
		t.Fatalf("bound the occupied port %d -- the test fixture is broken", taken)
	}
	if res.Persist {
		t.Errorf("Persist = true for a fallback port -- this is exactly how a configured port got overwritten in config.toml")
	}
	if res.FallbackReason == nil {
		t.Error("FallbackReason is nil -- the tray needs the bind error to log why it is not on the configured port")
	}
}

// TestListenPreferred_ConfiguredPortFreesDuringRetry_GetsIt covers the
// other common cause: a restart racing the previous instance's exit. A
// short retry should win the configured port rather than settling for a
// random one.
func TestListenPreferred_ConfiguredPortFreesDuringRetry_GetsIt(t *testing.T) {
	old := listenRetryInterval
	listenRetryInterval = 20 * time.Millisecond
	defer func() { listenRetryInterval = old }()

	blocker, taken := occupyPort(t)
	go func() {
		time.Sleep(120 * time.Millisecond)
		blocker.Close()
	}()

	res, err := ListenPreferred("127.0.0.1", taken, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Listener.Close()

	if res.Port != taken {
		t.Errorf("Port = %d, want the configured %d once it was released", res.Port, taken)
	}
	if res.Persist || res.FallbackReason != nil {
		t.Errorf("Persist=%v FallbackReason=%v, want false/nil -- the configured port was obtained", res.Persist, res.FallbackReason)
	}
}

// TestListenPreferred_NoConfiguredPort_PersistsTheAssignedOne keeps the
// original first-run behaviour: with nothing configured, cache whatever the
// OS picked so the dashboard's origin stays stable across restarts.
func TestListenPreferred_NoConfiguredPort_PersistsTheAssignedOne(t *testing.T) {
	res, err := ListenPreferred("127.0.0.1", 0, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Listener.Close()

	if res.Port == 0 {
		t.Error("Port = 0 -- nothing was bound")
	}
	if !res.Persist {
		t.Error("Persist = false with no configured port -- the first-run choice should be cached")
	}
}
