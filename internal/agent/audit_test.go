package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/guipguia/mnemo/internal/hub"
)

// Trimmed from what a kube-apiserver webhook backend posts at the RequestResponse level.
const auditEventList = `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[
{"level":"RequestResponse","stage":"ResponseComplete","verb":"patch",
 "user":{"username":"alice@example.com","groups":["sre"]},"userAgent":"kubectl/v1.34.1 (linux/amd64) kubernetes/abc",
 "objectRef":{"resource":"configmaps","namespace":"shop","name":"cfg","apiVersion":"v1"},
 "responseStatus":{"metadata":{},"code":200},
 "responseObject":{"kind":"ConfigMap","apiVersion":"v1","metadata":{"name":"cfg","namespace":"shop","resourceVersion":"812"}}},
{"level":"RequestResponse","stage":"ResponseComplete","verb":"create",
 "user":{"username":"system:serviceaccount:teleport:proxy"},"impersonatedUser":{"username":"bob"},"objectRef":{"resource":"configmaps","namespace":"shop"},
 "responseStatus":{"code":201},
 "responseObject":{"metadata":{"name":"generated-x7k2","namespace":"shop","resourceVersion":"813"}}},
{"level":"RequestResponse","stage":"ResponseComplete","verb":"delete","user":{"username":"carol"},
 "objectRef":{"resource":"configmaps","namespace":"shop","name":"old"},"responseStatus":{"code":200},
 "responseObject":{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Success",
  "details":{"name":"old","kind":"configmaps","uid":"6c1e-uid"}}},
{"level":"RequestResponse","stage":"RequestReceived","verb":"patch","user":{"username":"alice@example.com"},
 "objectRef":{"resource":"configmaps","namespace":"shop","name":"cfg"}},
{"level":"Metadata","stage":"ResponseComplete","verb":"update","user":{"username":"alice@example.com"},
 "objectRef":{"resource":"configmaps","namespace":"shop","name":"cfg"},"responseStatus":{"code":200}},
{"level":"RequestResponse","stage":"ResponseComplete","verb":"update","user":{"username":"alice@example.com"},
 "objectRef":{"resource":"configmaps","namespace":"shop","name":"cfg"},"responseStatus":{"code":409},
 "responseObject":{"kind":"Status","metadata":{}}},
{"level":"RequestResponse","stage":"ResponseComplete","verb":"patch","user":{"username":"system:node:n1"},
 "objectRef":{"resource":"pods","namespace":"shop","name":"p"},"responseStatus":{"code":200},
 "responseObject":{"metadata":{"name":"p","namespace":"shop","resourceVersion":"814"}}},
{"level":"RequestResponse","stage":"ResponseComplete","verb":"get","user":{"username":"alice@example.com"},
 "objectRef":{"resource":"configmaps","namespace":"shop","name":"cfg"},"responseStatus":{"code":200},
 "responseObject":{"metadata":{"name":"cfg","namespace":"shop","resourceVersion":"812"}}}
]}`

// selfSigned writes a throwaway certificate for 127.0.0.1 and returns its paths.
func selfSigned(t *testing.T) (cert, key string) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(k)
	cert, key = filepath.Join(t.TempDir(), "crt"), filepath.Join(t.TempDir(), "key")
	os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	return cert, key
}

func TestAuditWebhook(t *testing.T) {
	var got []hub.Batch
	cert, key := selfSigned(t)
	a := &Agent{Cluster: "prod", uid: "uid-1", AuditToken: "audit-secret", AuditCert: cert, AuditKey: key,
		Resources: []schema.GroupVersionResource{configmaps},
		Push:      func(_ context.Context, b hub.Batch) error { got = append(got, b); return nil }}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go a.serveAudit(t.Context(), l)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}

	post := func(token string) int {
		req, _ := http.NewRequest("POST", "https://"+l.Addr().String(), strings.NewReader(auditEventList))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post("wrong"); code != http.StatusUnauthorized || len(got) != 0 {
		t.Fatalf("bad token: %d, %d pushes", code, len(got))
	}
	if code := post("audit-secret"); code != http.StatusOK || len(got) != 1 {
		t.Fatalf("post: %d, %d pushes", code, len(got))
	}
	want := []hub.Audit{
		{Namespace: "shop", Name: "cfg", ResourceVersion: "812", User: "alice@example.com", UserAgent: "kubectl/v1.34.1 (linux/amd64) kubernetes/abc"},
		{Namespace: "shop", Name: "generated-x7k2", ResourceVersion: "813", User: "bob"},
		{Namespace: "shop", Name: "old", UID: "6c1e-uid", User: "carol"},
	}
	if b := got[0]; b.Cluster != "prod" || b.ClusterUID != "uid-1" || !slices.Equal(b.Audits, want) {
		t.Fatalf("pushed %+v, want audits %+v", b, want)
	}
}
