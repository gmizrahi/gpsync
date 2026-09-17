package dashboard

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gmizrahi/gpsync/internal/fsperm"
)

// The generated pair lives beside the ledger, so `gpsync backup` picks it up
// with everything else in ~/.gpsync -- see internal/backup, which archives
// that directory dynamically rather than from a file list.
const (
	TLSCertFileName = "dashboard_cert.pem"
	TLSKeyFileName  = "dashboard_key.pem"

	// tlsCertValidity is 825 days, the longest a leaf certificate is widely
	// accepted for. Browsers cap publicly-trusted chains at 398 days, but a
	// certificate the user has trusted by hand is exempt in practice, and
	// mkcert uses a comparable figure for the same reason. Longer would
	// risk a silent rejection; much shorter would mean re-trusting it by
	// hand every year, since a regenerated certificate has a new
	// fingerprint.
	tlsCertValidity = 825 * 24 * time.Hour

	// tlsRenewBefore regenerates while the old pair still works, rather
	// than at the moment it stops. The dashboard would otherwise refuse to
	// start on the first run after expiry.
	tlsRenewBefore = 30 * 24 * time.Hour
)

// ErrTLSFilesIncomplete is returned when only one of the cert/key pair is
// configured. Generating the missing half would silently pair a
// user-supplied certificate with a key gpsync invented, which cannot work
// and would look like a corrupt file rather than a configuration mistake.
var ErrTLSFilesIncomplete = errors.New("dashboard TLS needs both a certificate file and a key file, or neither")

// TLSFiles describes the certificate the dashboard should serve with.
type TLSFiles struct {
	CertPath string
	KeyPath  string

	// Fingerprint is the SHA-256 of the leaf certificate's DER bytes, in
	// the colon-separated uppercase hex form browsers show. It is the only
	// way a user can confirm the certificate their browser is warning
	// about is the one gpsync generated, rather than an interceptor's.
	Fingerprint string

	NotAfter time.Time

	// Generated is true when this call wrote a new pair. Supplied is true
	// when the paths came from config rather than being managed here.
	Generated bool
	Supplied  bool
}

// EnsureTLSFiles returns a usable certificate/key pair for the dashboard,
// generating a self-signed one under dir when none is configured.
//
// It is idempotent: with an existing pair that parses, still has life left,
// and covers every host asked for, it reads the fingerprint and returns
// without writing anything. It regenerates only when the pair is missing,
// unreadable, near expiry, or no longer covers the addresses the dashboard
// is reachable on -- the last of which matters because a machine's LAN
// address changes.
//
// certFile/keyFile override the managed pair entirely. Those are never
// written to or replaced: a certificate from mkcert, a home CA, or a real
// CA is the user's to manage, and silently regenerating over one would
// destroy a key gpsync did not create.
func EnsureTLSFiles(dir, certFile, keyFile string, hosts []string, now time.Time) (TLSFiles, error) {
	if (certFile == "") != (keyFile == "") {
		return TLSFiles{}, ErrTLSFilesIncomplete
	}

	if certFile != "" {
		leaf, err := loadLeaf(certFile, keyFile)
		if err != nil {
			return TLSFiles{}, fmt.Errorf("loading the configured dashboard certificate: %w", err)
		}
		return TLSFiles{
			CertPath:    certFile,
			KeyPath:     keyFile,
			Fingerprint: Fingerprint(leaf),
			NotAfter:    leaf.NotAfter,
			Supplied:    true,
		}, nil
	}

	certPath := filepath.Join(dir, TLSCertFileName)
	keyPath := filepath.Join(dir, TLSKeyFileName)

	// A parse failure here is deliberately not an error: a truncated or
	// half-written pair should be replaced, not turned into a startup
	// failure the user has to resolve by deleting files by hand.
	if leaf, err := loadLeaf(certPath, keyPath); err == nil && usable(leaf, hosts, now) {
		return TLSFiles{
			CertPath:    certPath,
			KeyPath:     keyPath,
			Fingerprint: Fingerprint(leaf),
			NotAfter:    leaf.NotAfter,
		}, nil
	}

	leaf, err := generateSelfSigned(dir, certPath, keyPath, hosts, now)
	if err != nil {
		return TLSFiles{}, err
	}
	return TLSFiles{
		CertPath:    certPath,
		KeyPath:     keyPath,
		Fingerprint: Fingerprint(leaf),
		NotAfter:    leaf.NotAfter,
		Generated:   true,
	}, nil
}

// Fingerprint renders a certificate's SHA-256 the way browsers display it,
// so the two can be compared by eye during the one-time trust prompt.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

// loadLeaf checks the pair actually works together, not just that both
// files parse -- a mismatched cert and key is a real way to get a server
// that binds and then fails every handshake.
func loadLeaf(certPath, keyPath string) (*x509.Certificate, error) {
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	if len(pair.Certificate) == 0 {
		return nil, errors.New("certificate file contained no certificate")
	}
	return x509.ParseCertificate(pair.Certificate[0])
}

func usable(leaf *x509.Certificate, hosts []string, now time.Time) bool {
	if now.After(leaf.NotAfter.Add(-tlsRenewBefore)) || now.Before(leaf.NotBefore) {
		return false
	}
	for _, h := range hosts {
		if leaf.VerifyHostname(h) != nil {
			return false
		}
	}
	return true
}

func generateSelfSigned(dir, certPath, keyPath string, hosts []string, now time.Time) (*x509.Certificate, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}

	// P-256 rather than RSA: universally supported by browsers, and the key
	// is generated in microseconds instead of seconds, which matters when
	// this runs on the startup path of a tray app.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "GPhotos Sync dashboard"},
		// Backdated an hour so a clock skew between this machine and the
		// browser cannot make a just-written certificate look not-yet-valid.
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(tlsCertValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		// Self-signed and self-issued: to trust this at all the user must
		// add it to their own trust store, and Windows will only accept a
		// certificate there as an anchor if it can act as one.
		IsCA: true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}

	// The key is written first and owner-only. If the process dies between
	// the two writes, the next start finds a pair that will not load and
	// regenerates both -- which is why loadLeaf failing is not fatal above.
	if err := writeOwnerOnly(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})); err != nil {
		return nil, err
	}
	// The certificate is public by design: it is what the browser is shown.
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil { //nolint:gosec // G306: a public certificate, not a secret
		return nil, err
	}

	return x509.ParseCertificate(der)
}

// writeOwnerOnly stages, renames, then restricts -- the same order
// auth.saveToken uses, and for the same reason: a rename does not carry the
// staging file's DACL to the target, so restricting before the rename would
// leave the surviving file readable by everyone on Windows.
func writeOwnerOnly(path string, data []byte) error {
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return fsperm.RestrictToOwner(path)
}

// DefaultCertHosts is every name and address the dashboard is plausibly
// reached on: loopback always, plus this machine's hostname and its
// non-loopback interface addresses, since a LAN bind is served to whatever
// address the browser happens to use.
//
// An interface that cannot be enumerated is skipped rather than failing --
// a certificate missing one address is far better than no dashboard.
func DefaultCertHosts() []string {
	hosts := []string{"localhost", "127.0.0.1", "::1"}
	seen := map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}

	if name, err := os.Hostname(); err == nil && name != "" && !seen[name] {
		hosts = append(hosts, name)
		seen[name] = true
	}

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return hosts
	}
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() || ipNet.IP.IsLinkLocalUnicast() {
			continue
		}
		s := ipNet.IP.String()
		if !seen[s] {
			hosts = append(hosts, s)
			seen[s] = true
		}
	}
	return hosts
}
