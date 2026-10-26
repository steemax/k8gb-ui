package main

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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFlagSafety(t *testing.T) {
	for _, args := range [][]string{
		{}, {"--insecure-local", "--listen=:0"}, {"--insecure-local", "--listen=0.0.0.0:0"},
		{"--insecure-local", "--listen=localhost:0"},
		{"--insecure-local", "--listen=127.0.0.1:0", "--tls-cert-file=cert"},
		{"--insecure-local", "--listen=127.0.0.1:0", "--reconcile"},
		{"--insecure-local", "--listen=127.0.0.1:0", "--kubeconfig=unused"},
		{"--insecure-local", "--listen=127.0.0.1:0", "--reconcile-interval=0s"},
	} {
		if err := run(context.Background(), args); err == nil {
			t.Fatalf("accepted unsafe flags %v", args)
		}
	}
}

func tlsFiles(t *testing.T) (string, string, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "webhook-test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk}), 0600); err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("test CA")
	}
	return certPath, keyPath, pool
}

func TestServerTLSReadinessWithoutKubernetesAndGracefulShutdown(t *testing.T) {
	cert, key, roots := tlsFiles(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		finished <- run(ctx, []string{"--listen=" + address, "--tls-cert-file=" + cert, "--tls-private-key-file=" + key})
	}()
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		response, err := client.Get("https://" + address + "/readyz")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode != 200 {
				t.Fatalf("readiness HTTP %d", response.StatusCode)
			}
			break
		}
		select {
		case err := <-finished:
			t.Fatalf("server exited: %v", err)
		case <-deadline:
			t.Fatal("server did not become ready")
		case <-ticker.C:
		}
	}
	response, err := client.Post("https://"+address+"/mutate", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 400 {
		t.Fatal("admission endpoint not available")
	}
	cancel()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not complete")
	}
}
