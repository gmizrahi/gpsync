package dashboard

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// maxLoginFailures/loginLockoutWindow bound how many bad /login attempts
// one client IP gets before being locked out for a while -- the project's
// own standing security rule ("Auth Failures — enforce rate limits on
// login/auth endpoints") was never implemented when the login page
// replaced HTTP Basic Auth, a real gap in brand-new auth code found by
// review. Deliberately simple (an in-memory counter, not anything
// persistent or distributed): this dashboard's own threat model is a LAN,
// not the public internet, and the goal is raising the cost of a
// brute-force guess past what's practical, not building a production auth
// service.
const (
	maxLoginFailures   = 5
	loginLockoutWindow = 15 * time.Minute
)

// loginAttemptTracker is created fresh by Handler on every call (never a
// package-level var) so two servers -- or two tests in the same process,
// each building their own httptest.Server -- never share lockout state.
type loginAttemptTracker struct {
	mu   sync.Mutex
	byIP map[string]*loginAttemptState
}

type loginAttemptState struct {
	count    int
	lastFail time.Time
}

func newLoginAttemptTracker() *loginAttemptTracker {
	return &loginAttemptTracker{byIP: make(map[string]*loginAttemptState)}
}

// locked reports whether ip currently has too many recent failures to
// even attempt another login -- checked BEFORE the (comparatively
// expensive, ~100ms at bcrypt.DefaultCost) credential check, so a
// lockout also caps the CPU a burst of guesses can burn, not just the
// guess count.
func (t *loginAttemptTracker) locked(ip string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.byIP[ip]
	if !ok {
		return false
	}
	if time.Since(s.lastFail) > loginLockoutWindow {
		delete(t.byIP, ip)
		return false
	}
	return s.count >= maxLoginFailures
}

// recordFailure increments ip's failure count -- a stale count (its last
// failure was outside the window) resets to 1 rather than compounding
// forever.
func (t *loginAttemptTracker) recordFailure(ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.byIP[ip]
	if !ok || time.Since(s.lastFail) > loginLockoutWindow {
		s = &loginAttemptState{}
		t.byIP[ip] = s
	}
	s.count++
	s.lastFail = time.Now()
}

// recordSuccess clears ip's failure history -- a real login shouldn't
// leave a lingering count from earlier mistyped attempts.
func (t *loginAttemptTracker) recordSuccess(ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.byIP, ip)
}

// clientIP extracts just the address from r.RemoteAddr ("ip:port"),
// since the port is a new, meaningless number on every connection and
// would defeat per-client tracking entirely. Falls back to the raw
// RemoteAddr on the rare malformed input rather than failing open to an
// empty/shared key.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
