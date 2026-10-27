package helmtest

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestReleaseImageDefaults(t *testing.T) {
	var chart struct {
		Version    string `json:"version"`
		AppVersion string `json:"appVersion"`
	}
	raw, err := os.ReadFile("../../charts/k8gb-ui/Chart.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, &chart); err != nil {
		t.Fatal(err)
	}
	version := regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?$`)
	if !version.MatchString(chart.Version) || !version.MatchString(chart.AppVersion) {
		t.Fatalf("expected release versions, got %+v", chart)
	}
	if requested := os.Getenv("RELEASE_VERSION"); requested != "" && requested != chart.AppVersion {
		t.Fatalf("requested image version %q does not match appVersion %q", requested, chart.AppVersion)
	}
	if os.Getenv("REQUIRE_UNPUBLISHED_CHART") == "1" {
		tag := "refs/tags/k8gb-ui-" + chart.Version
		err := exec.Command("git", "show-ref", "--verify", "--quiet", tag).Run()
		if err == nil {
			t.Fatalf("chart version %s is already released; bump Chart.yaml version", chart.Version)
		}
		if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
			t.Fatalf("cannot check chart release tag: %v", err)
		}
	}
	makefile, err := os.ReadFile("../../Makefile")
	if err != nil || !strings.Contains(string(makefile), "\nVERSION ?= "+chart.AppVersion+"\n") {
		t.Fatal("Makefile's default image version must match appVersion")
	}
	workflowRaw, err := os.ReadFile("../../.github/workflows/publish-images.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On map[string]struct {
			Inputs map[string]struct {
				Default string `json:"default"`
			} `json:"inputs"`
		} `json:"on"`
	}
	if err := yaml.Unmarshal(workflowRaw, &workflow); err != nil {
		t.Fatal(err)
	}
	if got := workflow.On["workflow_dispatch"].Inputs["version"].Default; got != chart.AppVersion {
		t.Fatalf("Publish images default version = %q, want appVersion %q", got, chart.AppVersion)
	}
	license, err := os.ReadFile("../../LICENSE")
	if err != nil || !bytes.Contains(license, []byte("Apache License")) {
		t.Fatal("missing project Apache-2.0 license")
	}
	chartLicense, err := os.ReadFile("../../charts/k8gb-ui/LICENSE")
	if err != nil || !bytes.Equal(chartLicense, license) {
		t.Fatal("chart and source must include the same project license")
	}
	var values map[string]any
	raw, err = os.ReadFile("../../charts/k8gb-ui/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	for _, component := range []string{"console", "agent", "webhook"} {
		image := values[component].(map[string]any)["image"].(map[string]any)
		if image["repository"] != "steemax/k8gb-ui" || image["tag"] != chart.AppVersion+"-"+component {
			t.Fatalf("wrong %s image: %+v", component, image)
		}
		if os.Getenv("CHECK_PUBLISHED_IMAGES") == "1" {
			ref := image["repository"].(string) + ":" + image["tag"].(string)
			out, err := exec.Command("docker", "buildx", "imagetools", "inspect", "--raw", ref).Output()
			if err != nil {
				t.Fatalf("published image %s is unavailable: %s: %v", ref, out, err)
			}
			var index struct {
				Manifests []struct {
					Platform struct {
						OS           string `json:"os"`
						Architecture string `json:"architecture"`
					} `json:"platform"`
				} `json:"manifests"`
			}
			if err := json.Unmarshal(out, &index); err != nil {
				t.Fatal(err)
			}
			platforms := map[string]bool{}
			for _, manifest := range index.Manifests {
				platforms[manifest.Platform.OS+"/"+manifest.Platform.Architecture] = true
			}
			if !platforms["linux/amd64"] || !platforms["linux/arm64"] {
				t.Fatalf("%s is missing a required architecture: %+v", ref, platforms)
			}
		}
	}
	for path, component := range map[string]string{"agent.yaml": "agent", "traefik-webhook.yaml": "webhook"} {
		raw, err := os.ReadFile(filepath.Join("../../deploy", path))
		if err != nil {
			t.Fatal(err)
		}
		for _, doc := range documents(t, raw) {
			if doc["kind"] != "Deployment" {
				continue
			}
			containers := doc["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)
			want := "steemax/k8gb-ui:" + chart.AppVersion + "-" + component
			if got := containers[0].(map[string]any)["image"]; got != want {
				t.Fatalf("%s image = %v, want %s", path, got, want)
			}
		}
	}
}

func TestChartImageOverrides(t *testing.T) {
	raw, err := render(t, "-f", "../../charts/k8gb-ui/examples/builtin.yaml", "--set", "console.image.repository=registry.example.test/team/console", "--set", "console.image.tag=custom")
	if err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	for _, doc := range documents(t, raw) {
		if doc["kind"] == "Deployment" {
			containers := doc["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)
			if image := containers[0].(map[string]any)["image"]; image != "registry.example.test/team/console:custom" {
				t.Fatalf("image override ignored: %v", image)
			}
			return
		}
	}
	t.Fatal("missing Deployment")
}

func TestPublicationWorkflowsAreManualAndPinned(t *testing.T) {
	paths, err := filepath.Glob("../../.github/workflows/*.yaml")
	if err != nil || len(paths) != 3 {
		t.Fatalf("expected CI and two publication workflows: %v, %v", paths, err)
	}
	pinned := regexp.MustCompile(`^[\w.-]+/[\w./-]+@[a-f0-9]{40}$`)
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var flow struct {
			On          map[string]any    `json:"on"`
			Permissions map[string]string `json:"permissions"`
			Jobs        map[string]struct {
				Steps []struct {
					Uses string `json:"uses"`
				} `json:"steps"`
			} `json:"jobs"`
		}
		if err := yaml.Unmarshal(raw, &flow); err != nil {
			t.Fatal(err)
		}
		if flow.Permissions["contents"] != "read" {
			t.Fatalf("%s must default to read-only", path)
		}
		if strings.HasPrefix(filepath.Base(path), "publish-") {
			if _, manual := flow.On["workflow_dispatch"]; !manual || len(flow.On) != 1 {
				t.Fatalf("%s publishes without a manual dispatch", path)
			}
		}
		for _, job := range flow.Jobs {
			for _, step := range job.Steps {
				if step.Uses != "" && !pinned.MatchString(step.Uses) {
					t.Fatalf("%s has an unpinned action: %s", path, step.Uses)
				}
			}
		}
	}
}
