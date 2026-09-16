package dashboard

import (
	"net"
	"strconv"
	"time"
)

// ListenResult is what ListenPreferred bound, and whether the caller should
// write the port back to config.
type ListenResult struct {
	Listener net.Listener
	Port     int
	// Persist is true only when no port was configured and the OS picked
	// one -- the first-run case, where caching the choice keeps the
	// dashboard's origin stable across restarts. It is NEVER true for a
	// port the user set, whatever happened while binding it.
	Persist bool
	// FallbackReason is the bind error that forced a temporary port, when
	// the configured one could not be used. Nil otherwise.
	FallbackReason error
}

// listenRetryInterval is how often ListenPreferred re-attempts a busy
// configured port. A package var so tests are not stuck with it.
var listenRetryInterval = 250 * time.Millisecond

// ListenPreferred binds the dashboard's listener, honouring a configured
// port without ever replacing it.
//
// It replaces a helper that fell back to an OS-assigned port on ANY bind
// failure and let the caller save that port over the configured one, so a
// configured port kept being replaced by a random one. The cause was not
// another process holding the port: on a
// Windows host running WSL2, Hyper-V reserves blocks of the dynamic range
// (49152-65535) and moves them on reboot, and the user's port 51133 had
// landed inside the reserved block 51095-51194. The bind was refused, a
// random port was picked, and config.toml was silently rewritten to it --
// so the choice was lost permanently, not just for one session.
//
// Now:
//   - preferred == 0: bind any port and report Persist, so it can be cached.
//   - preferred > 0: retry for up to retryFor, which covers a restart racing
//     the previous instance's exit. If it still cannot bind, serve on a
//     temporary port for this session only and report why -- the dashboard
//     must stay reachable, but config.toml keeps saying what the user chose.
func ListenPreferred(host string, preferred int, retryFor time.Duration) (ListenResult, error) {
	if preferred <= 0 {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
		if err != nil {
			return ListenResult{}, err
		}
		return ListenResult{Listener: ln, Port: ln.Addr().(*net.TCPAddr).Port, Persist: true}, nil
	}

	deadline := time.Now().Add(retryFor)
	var lastErr error
	for {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(preferred)))
		if err == nil {
			return ListenResult{Listener: ln, Port: preferred}, nil
		}
		lastErr = err
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(listenRetryInterval)
	}

	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return ListenResult{}, err
	}
	return ListenResult{Listener: ln, Port: ln.Addr().(*net.TCPAddr).Port, FallbackReason: lastErr}, nil
}
