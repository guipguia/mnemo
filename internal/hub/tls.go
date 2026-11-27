package hub

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"sync"
	"time"
)

// KeyPair loads a certificate and key, and reloads them whenever either file changes, so
// a certificate renewed in place (cert-manager, a rotated Secret) is picked up without a
// restart. A failed reload keeps the previous pair.
func KeyPair(certFile, keyFile string) (func() (*tls.Certificate, error), error) {
	var mu sync.Mutex
	var cur *tls.Certificate
	var stamp [2]time.Time
	load := func() (*tls.Certificate, error) {
		mu.Lock()
		defer mu.Unlock()
		var now [2]time.Time
		for i, f := range []string{certFile, keyFile} {
			if fi, err := os.Stat(f); err == nil {
				now[i] = fi.ModTime()
			}
		}
		if cur != nil && now == stamp {
			return cur, nil
		}
		c, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			if cur != nil {
				return cur, nil // mid-rotation (cert written, key not yet): retry next handshake
			}
			return nil, err
		}
		cur, stamp = &c, now
		return cur, nil
	}
	_, err := load()
	return load, err
}

// CAPool reads PEM certificates from file.
// ponytail: read once at startup; restart to trust a new CA.
func CAPool(file string) (*x509.CertPool, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("%s: no PEM certificates", file)
	}
	return p, nil
}

// AgentTLS is the agent listener's TLS config: every client must present a certificate
// signed by ca. AgentHandler then checks that it names the cluster it pushes for.
func AgentTLS(certFile, keyFile, caFile string) (*tls.Config, error) {
	pair, err := KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	ca, err := CAPool(caFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		ClientAuth:     tls.RequireAndVerifyClientCert,
		ClientCAs:      ca,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return pair() },
	}, nil
}

// ClientTLS is an agent's TLS config: trust caFile for the hub (empty: the system roots)
// and present certFile/keyFile (empty: no client certificate).
func ClientTLS(caFile, certFile, keyFile string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		ca, err := CAPool(caFile)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = ca
	}
	if (certFile == "") != (keyFile == "") {
		return nil, errors.New("client certificate and key go together")
	}
	if certFile != "" {
		pair, err := KeyPair(certFile, keyFile)
		if err != nil {
			return nil, err
		}
		cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return pair() }
	}
	return cfg, nil
}

// certNames reports whether r's verified client certificate names cluster, as its common
// name or one of its DNS names.
func certNames(r *http.Request, cluster string) bool {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || cluster == "" {
		return false
	}
	c := r.TLS.VerifiedChains[0][0]
	return c.Subject.CommonName == cluster || slices.Contains(c.DNSNames, cluster)
}
