package console

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"k8gb-ui/web"
)

// Opt-in fixture for tests/auth.browser.cjs; never connects to Kubernetes.
func TestBuiltinBrowserFixture(t *testing.T) {
	dir := os.Getenv("K8GB_AUTH_BROWSER_DIR")
	if dir == "" {
		t.Skip("browser fixture is opt-in")
	}
	auth, _ := builtinConfig(t)
	cfg, agents := Demo("127.0.0.1:8080")
	cfg.Auth = auth
	for i := range cfg.Clusters {
		cfg.Clusters[i].URL = "https://" + cfg.Clusters[i].ID + ".example"
		cfg.Clusters[i].BearerTokenFile = "unused-fixture-token"
	}
	srv := httptest.NewUnstartedServer(nil)
	defer srv.Close()
	cfg.Auth.PublicURL = "https://" + srv.Listener.Addr().String()
	handler, err := New(context.Background(), cfg, web.Assets, agents, true)
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = handler
	srv.StartTLS()
	raw, _ := json.Marshal(map[string]string{"url": srv.URL})
	if err := os.WriteFile(filepath.Join(dir, "ready.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(150 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			if _, err := os.Stat(filepath.Join(dir, "stop")); err == nil {
				return
			}
		case <-deadline.C:
			t.Fatal("browser fixture timed out")
		}
	}
}
