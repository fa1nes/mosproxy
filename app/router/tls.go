package router

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/IrineSistiana/mosproxy/internal/mlog"
	"github.com/IrineSistiana/mosproxy/internal/testutils"
)

func makeTlsConfig(cfg *TlsConfig, requireCert bool) (*tls.Config, error) {
	c := new(tls.Config)

	// load cert(s)
	if cfg.DebugUseTempCert {
		cert, err := testutils.GenerateCertificate("test.test")
		if err != nil {
			return nil, fmt.Errorf("failed to generate cert, %w", err)
		}
		c.Certificates = []tls.Certificate{cert}
	} else if len(cfg.Certs) > 0 || len(cfg.Keys) > 0 {
		if len(cfg.Certs) != len(cfg.Keys) {
			return nil, fmt.Errorf("mismatched certificates and keys, got %d cert(s) and %d key(s)", len(cfg.Certs), len(cfg.Keys))
		}

		ks := make([]*certKeeper, 0, len(cfg.Certs))
		for i := 0; i < len(cfg.Certs); i++ {
			k, err := newCertKeeper(cfg.Certs[i], cfg.Keys[i])
			if err != nil {
				return nil, err
			}
			ks = append(ks, k)
		}
		c.GetCertificate = (&certSet{keepers: ks}).get
	}

	if requireCert && len(c.Certificates) == 0 && c.GetCertificate == nil {
		return nil, errors.New("empty cert/key pair, tls config needs a valid certificate")
	}

	if len(cfg.CA) > 0 {
		pool, err := loadCA(cfg.CA)
		if err != nil {
			return nil, fmt.Errorf("failed to load ca %s, %w", cfg.CA, err)
		}
		c.RootCAs = pool
	}

	c.InsecureSkipVerify = cfg.InsecureSkipVerify
	return c, nil
}

func loadCA(f string) (*x509.CertPool, error) {
	caCert, err := os.ReadFile(f)
	if err != nil {
		return nil, err
	}
	caCertPool := x509.NewCertPool()
	ok := caCertPool.AppendCertsFromPEM(caCert)
	if !ok {
		return nil, fmt.Errorf("file seems not contain a valid cert")
	}
	return caCertPool, nil
}

// certReloadInterval bounds how often the on-disk certificate is stat'ed.
// GetCertificate runs on every handshake, so an unconditional stat there would
// put a syscall on the hot path. ACME renewals happen on a scale of days; a ten
// second detection delay is irrelevant, a per-handshake stat is not.
const certReloadInterval = 10 * time.Second

// certKeeper holds one certificate/key pair and reloads it when the files
// change on disk.
//
// Without this, certificates are read exactly once at startup and the only way
// to pick up a renewal is to restart the process. That is a poor trade for a
// DNS server: a restart drops every in-flight DoH/DoT connection, discards the
// whole in-memory cache, and resets the TLS session ticket keys, so every
// client is forced through a full handshake again. Deployments using
// short-lived certificates (Let's Encrypt IP certificates last about six days)
// pay that cost every few days.
type certKeeper struct {
	certFile, keyFile string

	mu      sync.RWMutex
	cert    *tls.Certificate
	certMod time.Time
	keyMod  time.Time
}

func newCertKeeper(certFile, keyFile string) (*certKeeper, error) {
	k := &certKeeper{certFile: certFile, keyFile: keyFile}
	if err := k.reload(); err != nil {
		return nil, fmt.Errorf("failed to load cert from %s and key %s, %w", certFile, keyFile, err)
	}
	return k, nil
}

func loadCertPair(certFile, keyFile string) (*tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	if len(cert.Certificate) > 0 && cert.Leaf == nil { // for go <= 1.22
		cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return nil, fmt.Errorf("failed to parse certificate leaf, %w", err)
		}
	}
	return &cert, nil
}

// reload re-reads the pair if either file's mtime moved. It is a no-op
// otherwise, which is the common case.
func (k *certKeeper) reload() error {
	certSt, err := os.Stat(k.certFile)
	if err != nil {
		return err
	}
	keySt, err := os.Stat(k.keyFile)
	if err != nil {
		return err
	}

	k.mu.RLock()
	unchanged := k.cert != nil &&
		certSt.ModTime().Equal(k.certMod) &&
		keySt.ModTime().Equal(k.keyMod)
	k.mu.RUnlock()
	if unchanged {
		return nil
	}

	cert, err := loadCertPair(k.certFile, k.keyFile)
	if err != nil {
		return err
	}

	k.mu.Lock()
	k.cert, k.certMod, k.keyMod = cert, certSt.ModTime(), keySt.ModTime()
	k.mu.Unlock()
	return nil
}

func (k *certKeeper) load() *tls.Certificate {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.cert
}

// certSet serves the GetCertificate callback for one listener.
type certSet struct {
	keepers []*certKeeper

	// lastCheck is the unix-nano timestamp of the most recent reload attempt.
	lastCheck atomic.Int64
}

func (s *certSet) refreshIfDue() {
	now := time.Now().UnixNano()
	last := s.lastCheck.Load()
	if now-last < int64(certReloadInterval) {
		return
	}
	// CAS so concurrent handshakes do not all stat the files.
	if !s.lastCheck.CompareAndSwap(last, now) {
		return
	}
	for _, k := range s.keepers {
		if err := k.reload(); err != nil {
			// Keep serving the previous certificate. ACME clients do not write
			// the cert and the key atomically, so a reload can legitimately
			// catch a truncated file, or a cert and key that do not match yet.
			// Failing handshakes over a transient write window would be worse
			// than serving a still-valid older certificate; the next attempt
			// picks up the new pair.
			mlog.L().Warn().
				Err(err).
				Str("cert", k.certFile).
				Str("key", k.keyFile).
				Msg("failed to reload certificate, keeping the previous one")
		}
	}
}

func (s *certSet) get(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
	s.refreshIfDue()
	for _, k := range s.keepers {
		cert := k.load()
		if cert == nil {
			continue
		}
		if err := chi.SupportsCertificate(cert); err == nil {
			return cert, nil
		}
	}
	// Mirror crypto/tls' behaviour for a static Certificates slice: when no
	// certificate matches the ClientHello, fall back to the first one rather
	// than aborting the handshake.
	if len(s.keepers) > 0 {
		if cert := s.keepers[0].load(); cert != nil {
			return cert, nil
		}
	}
	return nil, errors.New("no certificate available")
}
