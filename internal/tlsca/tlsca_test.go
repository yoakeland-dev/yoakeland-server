package tlsca_test

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"yoakeland/internal/tlsca"
)

func TestEnsureServerCert_GeneratesValidChain(t *testing.T) {
	stateDir := t.TempDir()
	sans := []string{"localhost", "127.0.0.1"}

	cert, caPEMPath, fp, err := tlsca.EnsureServerCert(stateDir, sans)
	if err != nil {
		t.Fatalf("EnsureServerCert: %v", err)
	}
	if fp == "" {
		t.Error("expected a non-empty CA fingerprint")
	}
	if _, err := os.Stat(caPEMPath); err != nil {
		t.Fatalf("expected CA PEM file at %s: %v", caPEMPath, err)
	}

	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parsing returned leaf certificate: %v", err)
	}

	caPEM, err := os.ReadFile(caPEMPath)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to load CA cert into pool")
	}

	if _, err := leaf.Verify(x509.VerifyOptions{
		DNSName: "localhost",
		Roots:   pool,
	}); err != nil {
		t.Fatalf("leaf certificate does not verify against its own CA: %v", err)
	}

	gotDNS := leaf.DNSNames
	if len(gotDNS) != 1 || gotDNS[0] != "localhost" {
		t.Errorf("leaf DNSNames = %v, want [localhost]", gotDNS)
	}
	if len(leaf.IPAddresses) != 1 || leaf.IPAddresses[0].String() != "127.0.0.1" {
		t.Errorf("leaf IPAddresses = %v, want [127.0.0.1]", leaf.IPAddresses)
	}
}

func TestEnsureServerCert_ReusesFilesForSameSANs(t *testing.T) {
	stateDir := t.TempDir()
	sans := []string{"localhost"}

	_, caPEMPath, fp1, err := tlsca.EnsureServerCert(stateDir, sans)
	if err != nil {
		t.Fatalf("first EnsureServerCert: %v", err)
	}
	leafPath := filepath.Join(stateDir, "tls", "leaf.pem")
	leafBytesBefore, err := os.ReadFile(leafPath)
	if err != nil {
		t.Fatal(err)
	}
	caBytesBefore, err := os.ReadFile(caPEMPath)
	if err != nil {
		t.Fatal(err)
	}

	_, _, fp2, err := tlsca.EnsureServerCert(stateDir, sans)
	if err != nil {
		t.Fatalf("second EnsureServerCert: %v", err)
	}
	if fp1 != fp2 {
		t.Errorf("CA fingerprint changed across calls with identical SANs: %s vs %s", fp1, fp2)
	}

	leafBytesAfter, err := os.ReadFile(leafPath)
	if err != nil {
		t.Fatal(err)
	}
	caBytesAfter, err := os.ReadFile(caPEMPath)
	if err != nil {
		t.Fatal(err)
	}

	if string(leafBytesBefore) != string(leafBytesAfter) {
		t.Error("leaf certificate was regenerated even though SANs didn't change")
	}
	if string(caBytesBefore) != string(caBytesAfter) {
		t.Error("CA certificate was regenerated even though it should be created only once")
	}
}

func TestEnsureServerCert_RegeneratesLeafOnlyWhenSANsChange(t *testing.T) {
	stateDir := t.TempDir()

	_, caPEMPath, fp1, err := tlsca.EnsureServerCert(stateDir, []string{"localhost"})
	if err != nil {
		t.Fatalf("first EnsureServerCert: %v", err)
	}
	caBytesBefore, err := os.ReadFile(caPEMPath)
	if err != nil {
		t.Fatal(err)
	}

	cert, _, fp2, err := tlsca.EnsureServerCert(stateDir, []string{"localhost", "192.168.50.7"})
	if err != nil {
		t.Fatalf("second EnsureServerCert with different SANs: %v", err)
	}
	if fp1 != fp2 {
		t.Errorf("CA fingerprint changed after only the leaf's SANs changed: %s vs %s", fp1, fp2)
	}

	caBytesAfter, err := os.ReadFile(caPEMPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(caBytesBefore) != string(caBytesAfter) {
		t.Error("CA certificate was regenerated when only the leaf's SANs should have changed")
	}

	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(leaf.IPAddresses) != 1 || leaf.IPAddresses[0].String() != "192.168.50.7" {
		t.Errorf("expected the regenerated leaf to carry the new SAN, got IPs %v", leaf.IPAddresses)
	}
}

func TestDefaultSANs_IncludesLocalhostLoopbackAndHostname(t *testing.T) {
	sans := tlsca.DefaultSANs()

	want := map[string]bool{"localhost": false, "127.0.0.1": false, "::1": false}
	for _, s := range sans {
		if _, ok := want[s]; ok {
			want[s] = true
		}
	}
	for k, found := range want {
		if !found {
			t.Errorf("expected DefaultSANs() to include %q, got %v", k, sans)
		}
	}
}
