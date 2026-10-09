// Package tlsca manages a private, self-signed certificate authority for
// yoakeland's optional TCP listener, so it can run TLS by default without
// depending on a publicly-trusted CA (which needs a real domain and doesn't
// fit a personal/LAN server) and without falling back to a TOFU/pinning
// scheme. A client trusts this one CA directly — a normal, non-default
// root-cert-pool configuration, the same way an internal/homelab CA works —
// rather than verifying against the public trust store or a bespoke
// first-connection-pins-the-cert scheme.
package tlsca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	caValidity   = 20 * 365 * 24 * time.Hour // ~20 years: a personal server's own private root
	leafValidity = 365 * 24 * time.Hour      // ~1 year: cheap to auto-regenerate, so kept short-lived

	dirMode = 0755
	keyMode = 0600
)

func tlsDir(stateDir string) string {
	return filepath.Join(stateDir, "tls")
}

// DefaultSANs returns the set of names/addresses the auto-generated server
// leaf certificate should cover: localhost, its IPs, the machine's hostname,
// and every non-loopback address found on a local interface — so the
// certificate is valid whether a client dials the box by hostname or by
// whichever LAN IP it currently has.
func DefaultSANs() []string {
	sans := []string{"localhost", "127.0.0.1", "::1"}

	if host, err := os.Hostname(); err == nil && host != "" {
		sans = append(sans, host)
	}

	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			ipNet, ok := a.(*net.IPNet)
			if !ok || ipNet.IP.IsLoopback() {
				continue
			}
			sans = append(sans, ipNet.IP.String())
		}
	}

	return dedupeSorted(sans)
}

func dedupeSorted(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// localRanges are the addresses the CA may always certify: loopback, the
// private and link-local ranges, and CGNAT (Tailscale and friends).
var localRanges = []string{"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "100.64.0.0/10", "::1/128", "fc00::/7", "fe80::/10"}

// permittedIPRanges is localRanges plus each other (public) address the
// machine had when the CA was made, as a single-address range.
func permittedIPRanges(ips []net.IP) []*net.IPNet {
	var out []*net.IPNet
	for _, r := range localRanges {
		_, n, _ := net.ParseCIDR(r)
		out = append(out, n)
	}
	for _, ip := range ips {
		if !ipInRanges(ip, out) {
			bits := 128
			if ip.To4() != nil {
				ip, bits = ip.To4(), 32
			}
			out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
		}
	}
	return out
}

func ipInRanges(ip net.IP, ranges []*net.IPNet) bool {
	for _, n := range ranges {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// permittedSANs drops the names ca's constraints wouldn't let it certify
// (say, a public IP the machine picked up after the CA was made): a leaf
// carrying one fails verification in every client, including for the names
// that are fine. A CA from before name constraints permits everything.
// A dropped name needs a fresh CA (delete <state-dir>/tls, re-pair).
func permittedSANs(ca *x509.Certificate, sans []string) []string {
	if len(ca.PermittedDNSDomains) == 0 && len(ca.PermittedIPRanges) == 0 {
		return sans
	}
	var out []string
	for _, san := range sans {
		if ip := net.ParseIP(san); ip != nil {
			if ipInRanges(ip, ca.PermittedIPRanges) {
				out = append(out, san)
			}
			continue
		}
		for _, d := range ca.PermittedDNSDomains {
			if strings.EqualFold(san, d) || strings.HasSuffix(strings.ToLower(san), "."+strings.ToLower(d)) {
				out = append(out, san)
				break
			}
		}
	}
	return out
}

// splitSANs separates a mixed SAN list into DNS names and IP addresses, the
// two fields x509.Certificate wants them in separately.
func splitSANs(sans []string) (dnsNames []string, ips []net.IP) {
	for _, s := range sans {
		if ip := net.ParseIP(s); ip != nil {
			ips = append(ips, ip)
			continue
		}
		dnsNames = append(dnsNames, s)
	}
	return dnsNames, ips
}

// EnsureServerCert loads the persisted CA+leaf pair under
// <stateDir>/tls/, generating whichever parts are missing, and regenerating
// the leaf if its embedded SAN list no longer matches sans (e.g. the
// machine's LAN IP changed) or it's expired/expiring soon. The CA itself is
// long-lived and only ever generated once. Returns a ready-to-use server
// certificate, the path to the CA's PEM file (hand this to a client for
// pairing — trust only this CA, not the public trust store), and the CA's
// SHA-256 fingerprint (for a startup log a human can compare against).
func EnsureServerCert(stateDir string, sans []string) (cert tls.Certificate, caPEMPath string, caFingerprint string, err error) {
	dir := tlsDir(stateDir)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return tls.Certificate{}, "", "", fmt.Errorf("creating tls state dir: %w", err)
	}

	caPEMPath = filepath.Join(dir, "ca.pem")
	caKeyPath := filepath.Join(dir, "ca.key")
	leafPEMPath := filepath.Join(dir, "leaf.pem")
	leafKeyPath := filepath.Join(dir, "leaf.key")

	caCert, caKey, err := loadOrCreateCA(caPEMPath, caKeyPath, sans)
	if err != nil {
		return tls.Certificate{}, "", "", fmt.Errorf("CA: %w", err)
	}

	leafCert, leafKey, err := loadOrCreateLeaf(leafPEMPath, leafKeyPath, caCert, caKey, permittedSANs(caCert, sans))
	if err != nil {
		return tls.Certificate{}, "", "", fmt.Errorf("leaf certificate: %w", err)
	}

	tlsCert, err := tls.X509KeyPair(certToPEM(leafCert), keyToPEM(leafKey))
	if err != nil {
		return tls.Certificate{}, "", "", fmt.Errorf("assembling tls.Certificate: %w", err)
	}

	return tlsCert, caPEMPath, fingerprint(caCert), nil
}

func loadOrCreateCA(certPath, keyPath string, sans []string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	if cert, key, err := loadCertKeyPair(certPath, keyPath); err == nil {
		return cert, key, nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generating CA key: %w", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}

	dnsNames, ips := splitSANs(sans)
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "yoakeland local CA", Organization: []string{"yoakeland"}},
		NotBefore:             now.Add(-time.Hour), // small clock-skew tolerance
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		// SPEC.md tells browser users to trust this CA in their OS, so it
		// must not be able to vouch for anything but this server: no
		// intermediates, and only this machine's names and local/private
		// addresses (RFC 5280 name constraints, critical). A leaked ca.key
		// then can't impersonate any real site.
		MaxPathLen:                  0,
		MaxPathLenZero:              true,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         dnsNames,
		PermittedIPRanges:           permittedIPRanges(ips),
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("creating CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing freshly created CA certificate: %w", err)
	}

	if err := writePEM(certPath, "CERTIFICATE", der, 0644); err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshaling CA key: %w", err)
	}
	if err := writePEM(keyPath, "EC PRIVATE KEY", keyDER, keyMode); err != nil {
		return nil, nil, err
	}

	return cert, key, nil
}

func loadOrCreateLeaf(certPath, keyPath string, caCert *x509.Certificate, caKey *ecdsa.PrivateKey, sans []string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	wantSANs := dedupeSorted(sans)

	if cert, key, err := loadCertKeyPair(certPath, keyPath); err == nil {
		if leafStillValid(cert, wantSANs) {
			return cert, key, nil
		}
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generating leaf key: %w", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}

	dnsNames, ips := splitSANs(wantSANs)
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "yoakeland", Organization: []string{"yoakeland"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(leafValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
		IPAddresses:  ips,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("creating leaf certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing freshly created leaf certificate: %w", err)
	}

	if err := writePEM(certPath, "CERTIFICATE", der, 0644); err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshaling leaf key: %w", err)
	}
	if err := writePEM(keyPath, "EC PRIVATE KEY", keyDER, keyMode); err != nil {
		return nil, nil, err
	}

	return cert, key, nil
}

// leafStillValid reports whether a persisted leaf cert is still safe to
// reuse: not expired-or-expiring-soon, and its SAN list matches wantSANs
// exactly (a changed LAN IP/hostname means a new cert is needed).
func leafStillValid(cert *x509.Certificate, wantSANs []string) bool {
	if time.Until(cert.NotAfter) < 30*24*time.Hour {
		return false
	}

	var got []string
	got = append(got, cert.DNSNames...)
	for _, ip := range cert.IPAddresses {
		got = append(got, ip.String())
	}
	got = dedupeSorted(got)

	if len(got) != len(wantSANs) {
		return false
	}
	for i := range got {
		if got[i] != wantSANs[i] {
			return false
		}
	}
	return true
}

func loadCertKeyPair(certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, err
	}

	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, nil, fmt.Errorf("%s: not a PEM file", certPath)
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", certPath, err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, fmt.Errorf("%s: not a PEM file", keyPath)
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", keyPath, err)
	}

	return cert, key, nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("generating certificate serial: %w", err)
	}
	return serial, nil
}

func writePEM(path, blockType string, der []byte, mode os.FileMode) error {
	buf := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(path, buf, mode); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

func certToPEM(cert *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
}

func keyToPEM(key *ecdsa.PrivateKey) []byte {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		// key was just parsed/generated successfully by us moments ago;
		// marshaling it back can't fail in practice.
		panic(fmt.Sprintf("tlsca: marshaling already-valid EC key: %v", err))
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}

// fingerprint is the CA certificate's SHA-256 over its raw DER bytes,
// hex-encoded and colon-separated — the standard human-comparable form.
func fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}
