package helmtest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"k8gb-ui/internal/console"
	"sigs.k8s.io/yaml"
)

func render(t *testing.T, extra ...string) ([]byte, error) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	args := append([]string{"template", "review", "../../charts/k8gb-ui", "--namespace", "ui-test"}, extra...)
	return exec.Command("helm", args...).CombinedOutput()
}

func documents(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var result []map[string]any
	for _, part := range bytes.Split(raw, []byte("\n---")) {
		var obj map[string]any
		if err := yaml.UnmarshalStrict(part, &obj); err != nil {
			t.Fatal(err)
		}
		if len(obj) > 0 {
			result = append(result, obj)
		}
	}
	return result
}

func TestConsoleProfiles(t *testing.T) {
	for _, oidc := range []bool{false, true} {
		args := []string{"-f", "../../charts/k8gb-ui/examples/builtin.yaml"}
		if oidc {
			args = append(args, "-f", "../../charts/k8gb-ui/examples/oidc.yaml")
		}
		raw, err := render(t, args...)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		found := false
		for _, obj := range documents(t, raw) {
			if obj["kind"] == "Secret" || obj["kind"] == "ClusterRole" {
				t.Fatal("Console must not create credentials or Kubernetes privileges")
			}
			if obj["kind"] == "ConfigMap" {
				found = true
				p := filepath.Join(t.TempDir(), "console.yaml")
				if err := os.WriteFile(p, []byte(obj["data"].(map[string]any)["console.yaml"].(string)), 0600); err != nil {
					t.Fatal(err)
				}
				cfg, err := console.LoadConfig(p)
				if err != nil {
					t.Fatal(err)
				}
				if cfg.WritesEnabled || cfg.Auth.LocalAdmin {
					t.Fatal("default Console must be read-only and authenticated")
				}
				if oidc && cfg.Auth.Mode != "oidc" {
					t.Fatal("wrong auth mode")
				}
			}
		}
		if !found {
			t.Fatal("missing Console config")
		}
	}
}

func TestAgentAndWebhookProfiles(t *testing.T) {
	for _, writes := range []bool{false, true} {
		args := []string{"-f", "../../charts/k8gb-ui/examples/agent.yaml"}
		if writes {
			args = append(args, "-f", "../../charts/k8gb-ui/examples/writes.yaml", "--set", "agent.nativeSiteTraffic=true", "-f", "../../charts/k8gb-ui/examples/traefik.yaml", "--set", "webhook.caBundle=Y2E=")
		}
		raw, err := render(t, args...)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		for _, obj := range documents(t, raw) {
			if obj["kind"] == "ConfigMap" || obj["kind"] == "Secret" {
				t.Fatal("agent chart must not own mutable journals or credentials")
			}
			if obj["kind"] == "Role" || obj["kind"] == "ClusterRole" {
				for _, rule := range obj["rules"].([]any) {
					raw, _ := json.Marshal(rule)
					if bytes.Contains(raw, []byte(`"secrets"`)) || bytes.Contains(raw, []byte(`"*"`)) {
						t.Fatal("overbroad privilege")
					}
					if !writes && (bytes.Contains(raw, []byte(`"patch"`)) || bytes.Contains(raw, []byte(`"create"`))) {
						t.Fatal("read-only profile grants writes")
					}
				}
			}
			if obj["kind"] == "MutatingWebhookConfiguration" {
				hook := obj["webhooks"].([]any)[0].(map[string]any)
				if hook["sideEffects"] != "None" || hook["failurePolicy"] != "Fail" {
					t.Fatal("unsafe admission settings")
				}
			}
		}
	}
}

func TestRejectUnsafeChartValues(t *testing.T) {
	for _, override := range []string{"console.auth.mode=local", "console.auth.publicURL=http://ui.example.test", "console.clusters[0].url=http://agent.example.test", "console.replicas=0"} {
		raw, err := render(t, "-f", "../../charts/k8gb-ui/examples/builtin.yaml", "--set", override)
		if err == nil {
			t.Fatalf("accepted %s: %s", override, raw)
		}
	}
	for _, override := range []string{"agent.replicas=2", "agent.nativeSiteTraffic=true", "webhook.enabled=true", "agent.namespaces={}"} {
		raw, err := render(t, "-f", "../../charts/k8gb-ui/examples/agent.yaml", "--set", override)
		if err == nil {
			t.Fatalf("accepted %s: %s", override, raw)
		}
	}
	raw, err := render(t, "-f", "../../charts/k8gb-ui/examples/builtin.yaml", "--set", "console.authSecretName=")
	if err == nil || !strings.Contains(string(raw), "authSecretName") {
		t.Fatalf("expected missing secret failure: %s", raw)
	}
}

func TestPackagedChartExcludesLocalCredentials(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	dir := t.TempDir()
	chart := filepath.Join(dir, "chart")
	if err := os.CopyFS(chart, os.DirFS("../../charts/k8gb-ui")); err != nil {
		t.Fatal(err)
	}
	privateFiles := []string{".env", ".env.local", "client.key", "ca.pem", "ca.crt", "agent.token", "review.kubeconfig", "credentials.txt", "private-values.yaml", "templates/client.key", "AGENTS.md", "templates/AGENTS.md", "CLAUDE.md", "GEMINI.md", ".codex/notes.md"}
	for _, name := range privateFiles {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(chart, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(chart, name), []byte("packaging-test-canary\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if raw, err := exec.Command("helm", "package", chart, "--destination", dir).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	archives, err := filepath.Glob(filepath.Join(dir, "*.tgz"))
	if err != nil || len(archives) != 1 {
		t.Fatalf("expected one chart archive: %v, %v", archives, err)
	}
	file, err := os.Open(archives[0])
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	zipped, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer zipped.Close()
	reader := tar.NewReader(zipped)
	entries := map[string]bool{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		name := strings.TrimPrefix(header.Name, "k8gb-ui/")
		entries[name] = true
		for _, private := range privateFiles {
			if name == private {
				t.Fatalf("packaged private file %q", name)
			}
		}
	}
	for _, name := range []string{"Chart.yaml", "values.yaml", "values.schema.json", "README.md", "LICENSE", "templates/agent.yaml", "examples/builtin.yaml"} {
		if !entries[name] {
			t.Errorf("missing published chart file %q", name)
		}
	}
}
