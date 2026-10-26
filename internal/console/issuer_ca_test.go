package console

import (
	"context"
	"encoding/pem"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOIDCIssuerCA(t *testing.T) {
	p := newAuthTestProvider(t)
	// The fixture's convenience transport must not mask a missing custom CA.
	http.DefaultTransport = &http.Transport{}
	cfg := p.config(t)
	if _, err := newAuthenticator(context.Background(), cfg); err == nil {
		t.Fatal("untrusted issuer accepted without explicit CA")
	}
	cfg.IssuerCAFile = filepath.Join(t.TempDir(), "issuer.pem")
	if _, err := newAuthenticator(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "cannot read") {
		t.Fatal("missing CA must fail closed")
	}
	if err := os.WriteFile(cfg.IssuerCAFile, []byte("not a certificate"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := newAuthenticator(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "no certificates") {
		t.Fatal("invalid CA must fail closed")
	}
	if err := os.WriteFile(cfg.IssuerCAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := newAuthenticator(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	transport := a.client.Transport.(*http.Transport)
	if transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("custom CA must not disable certificate verification")
	}
	// Complete token exchange and JWKS verification with this same transport.
	cookie, state := p.begin(t, a)
	w := p.callback(a, cookie, state.State)
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Fatalf("callback failed with custom issuer CA: %d", w.Code)
	}
}
