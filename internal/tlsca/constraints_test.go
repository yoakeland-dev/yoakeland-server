package tlsca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"math/big"
	"path/filepath"
	"testing"
	"time"
)

// The CA gets imported into OS trust stores for the web UI, so a leaked
// ca.key must not be able to vouch for any other site.
func TestCA_NameConstrained(t *testing.T) {
	stateDir := t.TempDir()
	sans := []string{"localhost", "127.0.0.1", "myhost", "203.0.113.5", "192.168.1.9"}
	cert, _, _, err := EnsureServerCert(stateDir, sans)
	if err != nil {
		t.Fatal(err)
	}
	dir := tlsDir(stateDir)
	ca, caKey, err := loadCertKeyPair(filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca.key"))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	leaf, _ := x509.ParseCertificate(cert.Certificate[0])
	for _, name := range sans {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: name}); err != nil {
			t.Errorf("own leaf rejected for %s: %v", name, err)
		}
	}

	for _, rogue := range []*x509.Certificate{
		{DNSNames: []string{"example.com"}},
		{DNSNames: []string{"accounts.google.com"}},
	} {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		rogue.SerialNumber = big.NewInt(2)
		rogue.NotBefore, rogue.NotAfter = time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
		rogue.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		der, err := x509.CreateCertificate(rand.Reader, rogue, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		c, _ := x509.ParseCertificate(der)
		if _, err := c.Verify(x509.VerifyOptions{Roots: pool, DNSName: rogue.DNSNames[0]}); err == nil {
			t.Errorf("CA vouched for %s", rogue.DNSNames[0])
		}
	}

	if got := permittedSANs(ca, []string{"localhost", "8.8.8.8", "evil.com"}); len(got) != 1 || got[0] != "localhost" {
		t.Errorf("permittedSANs = %v", got)
	}
}
