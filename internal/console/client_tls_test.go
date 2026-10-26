package console

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "k8gb-ui/api/v1"
)

func TestRemoteUsesVerifiedServerCAAndClientCertificate(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test agent CA"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(serial int64, client bool) ([]byte, []byte) {
		t.Helper()
		leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "test agent transport"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
		if client {
			template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		} else {
			template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
			template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		}
		der, err := x509.CreateCertificate(rand.Reader, template, ca, &leafKey.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		private, err := x509.MarshalPKCS8PrivateKey(leafKey)
		if err != nil {
			t.Fatal(err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})
	}
	serverPEM, serverKey := issue(2, false)
	clientPEM, clientKey := issue(3, true)
	certificate, err := tls.X509KeyPair(serverPEM, serverKey)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	token := strings.Repeat("test-token-", 4)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(401)
			return
		}
		writeJSON(w, 200, v1.NamespaceList{APIVersion: v1.Version, ClusterID: "east", Namespaces: []string{"demo"}})
	}))
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	srv.StartTLS()
	defer srv.Close()
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	cfg := ClusterConfig{ID: "east", URL: srv.URL, BearerTokenFile: write("token", []byte(token)), CAFile: write("ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})), ClientCertFile: write("client.crt", clientPEM), ClientKeyFile: write("client.key", clientKey)}
	agent, err := newRemoteAgent(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if namespaces, err := agent.Namespaces(context.Background()); err != nil || len(namespaces) != 1 {
		t.Fatalf("mTLS request failed: namespaces=%v error=%v", namespaces, err)
	}
	cfg.ClientCertFile = ""
	cfg.ClientKeyFile = ""
	withoutCert, err := newRemoteAgent(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := withoutCert.Namespaces(context.Background()); err == nil {
		t.Fatal("request succeeded without required client certificate")
	}
	cfg.CAFile = ""
	cfg.ClientCertFile = filepath.Join(dir, "client.crt")
	cfg.ClientKeyFile = filepath.Join(dir, "client.key")
	untrusted, err := newRemoteAgent(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := untrusted.Namespaces(context.Background()); err == nil {
		t.Fatal("untrusted server certificate accepted")
	}
}
