package helmtest

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"k8gb-ui/internal/console"
	"sigs.k8s.io/yaml"
)

func readmeInstall(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	return markdownSection(t, string(raw), "## Install in Kubernetes\n", "## Deployment\n")
}

func markdownSection(t *testing.T, content, start, end string) string {
	t.Helper()
	i := strings.Index(content, start)
	if i < 0 {
		t.Fatalf("missing documentation marker %q", start)
	}
	content = content[i+len(start):]
	if end == "" {
		return content
	}
	j := strings.Index(content, end)
	if j < 0 {
		t.Fatalf("missing documentation marker %q", end)
	}
	return content[:j]
}

func firstFence(t *testing.T, content, language string) string {
	t.Helper()
	return markdownSection(t, content, "```"+language+"\n", "\n```")
}

func TestREADMEInstallationValues(t *testing.T) {
	install := readmeInstall(t)
	dir := t.TempDir()
	for _, name := range []string{"agent.yaml", "east.yaml", "west.yaml", "console.yaml"} {
		body := markdownSection(t, install, "Create `$PRIVATE/"+name+"`", "")
		if err := os.WriteFile(filepath.Join(dir, name), []byte(firstFence(t, body, "yaml")), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, site := range []string{"east", "west"} {
		t.Run(site, func(t *testing.T) {
			args := []string{"-f", filepath.Join(dir, "agent.yaml")}
			if site == "east" {
				args = append(args, "-f", filepath.Join(dir, "console.yaml"))
			}
			args = append(args, "-f", filepath.Join(dir, site+".yaml"))
			raw, err := render(t, args...)
			if err != nil {
				t.Fatalf("README %s values: %s: %v", site, raw, err)
			}
			foundAgent, foundConsole := false, false
			for _, obj := range documents(t, raw) {
				if obj["kind"] == "Secret" || obj["kind"] == "MutatingWebhookConfiguration" {
					t.Fatal("first install must not create credentials or enable the webhook")
				}
				if obj["kind"] == "Deployment" {
					name := obj["metadata"].(map[string]any)["name"]
					pod := obj["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
					container := pod["containers"].([]any)[0].(map[string]any)
					if name == "k8gb-ui-agent" {
						foundAgent = true
						joined := ""
						for _, arg := range container["args"].([]any) {
							joined += arg.(string) + "\n"
						}
						if !strings.Contains(joined, "--cluster-id="+site+"\n") || strings.Contains(joined, "--enable-writes") {
							t.Fatalf("wrong first-install agent configuration: %s", joined)
						}
						assertSecretMount(t, pod, container, "auth", "agent-auth", "/run/auth")
					}
					if name == "k8gb-ui-console" {
						foundConsole = true
						assertSecretMount(t, pod, container, "auth", "ui-auth", "/run/auth")
						for _, peer := range []string{"east", "west"} {
							assertSecretMount(t, pod, container, peer, peer+"-connection", "/run/sites/"+peer)
						}
					}
				}
				if obj["kind"] == "ConfigMap" {
					var cfg console.Config
					body := obj["data"].(map[string]any)["console.yaml"].(string)
					if err := yaml.UnmarshalStrict([]byte(body), &cfg); err != nil {
						t.Fatal(err)
					}
					if err := cfg.Validate(); err != nil {
						t.Fatal(err)
					}
					if cfg.WritesEnabled || cfg.Auth.Mode != "builtin" || len(cfg.Clusters) != 2 {
						t.Fatalf("wrong first-install Console scope: %+v", cfg)
					}
					for _, c := range cfg.Clusters {
						if c.BearerTokenFile != "/run/sites/"+c.ID+"/token" || c.WriteTokenFile != "" || c.CAFile != "" {
							t.Fatalf("default credentials must be mounted read tokens with system CA trust: %+v", c)
						}
					}
				}
			}
			if !foundAgent || foundConsole != (site == "east") {
				t.Fatalf("wrong placement: agent=%v, Console=%v", foundAgent, foundConsole)
			}
		})
	}
}

func assertSecretMount(t *testing.T, pod, container map[string]any, volumeName, secretName, mountPath string) {
	t.Helper()
	foundVolume, foundMount := false, false
	for _, v := range pod["volumes"].([]any) {
		volume := v.(map[string]any)
		if volume["name"] == volumeName {
			secret, ok := volume["secret"].(map[string]any)
			foundVolume = ok && secret["secretName"] == secretName
		}
	}
	for _, v := range container["volumeMounts"].([]any) {
		mount := v.(map[string]any)
		if mount["name"] == volumeName && mount["mountPath"] == mountPath && mount["readOnly"] == true {
			foundMount = true
		}
	}
	if !foundVolume || !foundMount {
		t.Fatalf("missing read-only Secret mount %s -> %s", secretName, mountPath)
	}
}

func TestREADMEAuthenticationSecretCommands(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	section := markdownSection(t, readmeInstall(t), "### 4. Create authentication and connection Secrets\n", "### 5. Write the values files\n")
	dir := t.TempDir()
	capture := filepath.Join(dir, "calls")
	// Capture arguments without contacting Kubernetes or exposing any credentials.
	wrapper := "#!/bin/sh\nprintf '%s\\000' \"$@\" >> \"$CAPTURE\"\nprintf '\\n' >> \"$CAPTURE\"\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-eu", "-c", firstFence(t, section, "sh"))
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "CAPTURE="+capture, "PRIVATE="+dir, "EAST_CONTEXT=east-context", "WEST_CONTEXT=west-context")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("README Secret commands: %s: %v", out, err)
	}
	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSuffix(raw, []byte("\n")), []byte("\n"))
	want := []struct {
		context, name string
		files         []string
	}{
		{"east-context", "agent-auth", []string{"token=east/token", "write-token=east/write-token", "plan-key=east/plan-key"}},
		{"west-context", "agent-auth", []string{"token=west/token", "write-token=west/write-token", "plan-key=west/plan-key"}},
		{"east-context", "ui-auth", []string{"users.yaml=users.yaml", "session-key=session-key"}},
		{"east-context", "east-connection", []string{"token=east/token", "write-token=east/write-token"}},
		{"east-context", "west-connection", []string{"token=west/token", "write-token=west/write-token"}},
	}
	if len(lines) != len(want) {
		t.Fatalf("expected %d Secret commands, got %d", len(want), len(lines))
	}
	for i, w := range want {
		expected := []string{"--context", w.context, "-n", "k8gb-ui", "create", "secret", "generic", w.name}
		for _, file := range w.files {
			key, value, _ := strings.Cut(file, "=")
			expected = append(expected, "--from-file="+key+"="+filepath.Join(dir, value))
		}
		got := strings.Split(strings.TrimSuffix(string(lines[i]), "\x00"), "\x00")
		if strings.Join(got, "\x00") != strings.Join(expected, "\x00") {
			t.Fatalf("Secret command %d does not match the values contract: %q", i+1, got)
		}
	}
}
