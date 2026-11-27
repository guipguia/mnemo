package hub

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/guipguia/mnemo/internal/store"
)

// testCA issues certificates for the agent listener tests and writes them as PEM files.
type testCA struct {
	t    *testing.T
	dir  string
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newTestCA(t *testing.T) *testCA {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	ca := &testCA{t, t.TempDir(), cert, key}
	os.WriteFile(filepath.Join(ca.dir, "ca.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	return ca
}

// issue writes name.crt and name.key: a server certificate for 127.0.0.1, or a client one.
func (ca *testCA) issue(name, cn string, dns []string, server, noEKU bool) (certFile, keyFile string) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		DNSNames: dns, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	if noEKU {
		tmpl.ExtKeyUsage = nil
	}
	if server {
		tmpl.ExtKeyUsage, tmpl.IPAddresses = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, []net.IP{net.IPv4(127, 0, 0, 1)}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		ca.t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	certFile, keyFile = filepath.Join(ca.dir, name+".crt"), filepath.Join(ca.dir, name+".key")
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	// Renewals in the same second must still look new to KeyPair.
	later := time.Now().Add(time.Duration(tmpl.SerialNumber.Int64() % 1e9))
	os.Chtimes(certFile, later, later)
	os.Chtimes(keyFile, later, later)
	return certFile, keyFile
}

func TestAgentListener(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "mnemo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ca := newTestCA(t)
	sc, sk := ca.issue("hub", "hub", nil, true, false)
	tc, err := AgentTLS(sc, sk, filepath.Join(ca.dir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	// Not StartTLS: it installs its own certificate, which would shadow GetCertificate.
	srv := httptest.NewUnstartedServer(AgentHandler(st, "secret"))
	srv.Listener = tls.NewListener(srv.Listener, tc)
	srv.Start()
	defer srv.Close()
	url := strings.Replace(srv.URL, "http://", "https://", 1)

	client := func(certFile, keyFile string) *Client {
		t.Helper()
		ct, err := ClientTLS(filepath.Join(ca.dir, "ca.crt"), certFile, keyFile)
		if err != nil {
			t.Fatal(err)
		}
		return NewClient(url, "secret", ct)
	}
	push := func(c *Client, cluster string) error {
		return c.Push(t.Context(), Batch{Cluster: cluster, ClusterUID: "uid-" + cluster})
	}

	pc, pk := ca.issue("prod", "prod", nil, false, false)
	prod := client(pc, pk)
	if err := push(prod, "prod"); err != nil {
		t.Errorf("prod pushing prod: %v", err)
	}
	if _, err := prod.Resume(t.Context(), "prod", "uid-prod", ""); err != nil {
		t.Errorf("prod resuming prod: %v", err)
	}
	if err := push(prod, "staging"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("prod pushing staging: %v, want 403", err)
	}
	if _, err := prod.Resume(t.Context(), "staging", "uid-staging", ""); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("prod resuming staging: %v, want 403", err)
	}
	dc, dk := ca.issue("dns", "agent", []string{"staging"}, false, false) // the name can be a DNS SAN
	if err := push(client(dc, dk), "staging"); err != nil {
		t.Errorf("DNS name: %v", err)
	}
	nc, nk := ca.issue("noeku", "prod", nil, false, true)
	if err := push(client(nc, nk), "prod"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("certificate without client-auth usage: %v, want 403", err)
	}
	if err := push(client("", ""), "prod"); err == nil {
		t.Error("no client certificate: accepted")
	}
	other := newTestCA(t)
	oc, ok := other.issue("prod", "prod", nil, false, false)
	if err := push(client(oc, ok), "prod"); err == nil {
		t.Error("certificate from another CA: accepted")
	}

	// Renewed in place: the same files now name staging, and the client picks it up.
	ca.issue("prod", "staging", nil, false, false)
	prod.http.CloseIdleConnections() // a new handshake
	if err := push(prod, "staging"); err != nil {
		t.Errorf("after renewal: %v", err)
	}
}
