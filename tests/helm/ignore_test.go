package helmtest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitIgnoreKeepsPublicDocumentation(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	// Exercise Git's actual matching rules in a disposable fixture, never the checkout.
	dir := t.TempDir()
	for _, name := range []string{".gitignore", ".gitattributes"} {
		raw, err := os.ReadFile("../../" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if raw, err := exec.Command("git", "-C", dir, "init", "-q", "--template=", "--initial-branch=main").CombinedOutput(); err != nil {
		t.Fatalf("fixture init: %s: %v", raw, err)
	}
	private := []string{
		"AGENTS.md", "docs/AGENTS.md", ".codex/state.json", "tools/.claude/settings.json", "CLAUDE.md", "GEMINI.md",
		"artifacts/report.md", "dist/charts/chart.tgz", "docs/publication-readiness.md", "docs/migration.md",
		".cr-index/index.yaml", "runtime/state.json", "private/values.yaml", "console.yaml", "users.yaml", "session-key",
		"auth/client.key", "auth/ca.crt", "auth/secret.token", ".env.local", "review.kubeconfig", "storage-state.json",
		"private-values.yaml", "deploy/values.private.yaml", "values.local.yaml", "agent.log", ".aider.chat.history.md",
		".github/copilot-instructions.md", "node_modules/dependency/index.js",
	}
	public := []string{
		"README.md", "LICENSE", "SECURITY.md", "CONTRIBUTING.md", "docs/releases.md", "docs/authentication.md",
		"docs/architecture.md", "docs/media/demo-preview.jpg", "docs/media/k8gb-ui-demo.mp4",
		"charts/k8gb-ui/README.md", "charts/k8gb-ui/LICENSE", "web/static/vendor/lucide.LICENSE",
		".env.example", ".github/workflows/ci.yaml", "tests/helm/ignore_test.go",
	}
	cmd := exec.Command("git", "-C", dir, "-c", "core.excludesFile=/dev/null", "check-ignore", "--no-index", "--stdin")
	cmd.Stdin = strings.NewReader(strings.Join(append(append([]string{}, private...), public...), "\n") + "\n")
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("check-ignore: %s: %v", raw, err)
	}
	ignored := map[string]bool{}
	for _, name := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		ignored[name] = true
	}
	for _, name := range private {
		if !ignored[name] {
			t.Errorf("private file is not ignored: %s", name)
		}
	}
	for _, name := range public {
		if ignored[name] {
			t.Errorf("public file is ignored: %s", name)
		}
	}
}
