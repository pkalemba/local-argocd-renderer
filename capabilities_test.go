package renderer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const capabilitiesApp = "examples/helm-capabilities/app.yaml"
const capabilitiesFile = "examples/helm-capabilities/capabilities.yaml"

// renderedObjects renders opts and returns what came out, keyed by name. The
// example is a Helm chart, so there is nothing to assert without helm.
func renderedObjects(t *testing.T, opts TemplateOptions) map[string]*unstructured.Unstructured {
	t.Helper()

	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}

	result, err := Template(context.Background(), opts)
	if err != nil {
		t.Fatalf("Failed to template application: %v", err)
	}

	objects := map[string]*unstructured.Unstructured{}
	for _, obj := range result.Objects {
		objects[obj.GetName()] = obj
	}

	return objects
}

func configMapData(t *testing.T, obj *unstructured.Unstructured, key string) string {
	t.Helper()

	value, found, err := unstructured.NestedString(obj.Object, "data", key)
	if err != nil || !found {
		t.Fatalf("Expected data.%s on %s, got %v (err %v)", key, obj.GetName(), obj.Object["data"], err)
	}

	return value
}

// The whole point of the file: a chart that guards a resource behind a CRD the
// cluster has renders that resource, and one that reads the cluster's version
// sees the version the file names rather than the helm binary's built-in default.
func TestHelmCapabilitiesReachHelmTemplate(t *testing.T) {
	objects := renderedObjects(t, TemplateOptions{
		ApplicationFile:      capabilitiesApp,
		RepoRoot:             ".",
		HelmCapabilitiesFile: capabilitiesFile,
	})

	reported, ok := objects["capabilities-example-app-capabilities"]
	if !ok {
		t.Fatalf("Expected the chart's ConfigMap to be rendered, got %v", objectNames(objects))
	}

	for key, want := range map[string]string{
		"kubeVersion":           "v1.31.4",
		"major":                 "1",
		"minor":                 "31",
		"hasPrometheusOperator": "true",
	} {
		if got := configMapData(t, reported, key); got != want {
			t.Errorf("Expected .Capabilities to report %s=%q, got %q", key, want, got)
		}
	}

	if _, ok := objects["capabilities-example-app-metrics"]; !ok {
		t.Errorf("Expected the ServiceMonitor guarded by APIVersions.Has to be rendered, got %v", objectNames(objects))
	}
}

// Without the file nothing changes: Helm's own defaults still decide, so the
// chart renders as if the CRD were not installed.
func TestWithoutHelmCapabilitiesTheChartFallsBackToHelmsDefaults(t *testing.T) {
	objects := renderedObjects(t, TemplateOptions{
		ApplicationFile: capabilitiesApp,
		RepoRoot:        ".",
	})

	reported, ok := objects["capabilities-example-app-capabilities"]
	if !ok {
		t.Fatalf("Expected the chart's ConfigMap to be rendered, got %v", objectNames(objects))
	}

	if got := configMapData(t, reported, "hasPrometheusOperator"); got != "false" {
		t.Errorf("Expected the CRD to be absent without a capabilities file, got %q", got)
	}

	if _, ok := objects["capabilities-example-app-metrics"]; ok {
		t.Error("Expected the ServiceMonitor to be left out without a capabilities file")
	}
}

// The file stands in for the destination cluster, and Argo CD lets a source pin
// its own version instead of taking the cluster's. Rendering has to agree with
// it, or a chart that pins one would be diffed against the wrong output.
func TestSourceKubeVersionWinsOverTheCapabilitiesFile(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}

	manifest := `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: capabilities-example-app
spec:
  project: default
  source:
    repoURL: https://github.com/myorg/myrepo
    path: examples/helm-capabilities/input
    helm:
      kubeVersion: v1.24.0
  destination:
    server: https://kubernetes.default.svc
    namespace: capabilities-namespace
`

	result, err := TemplateFromApplicationYAML(context.Background(), manifest, TemplateOptions{
		RepoRoot:             ".",
		HelmCapabilitiesFile: capabilitiesFile,
	})
	if err != nil {
		t.Fatalf("Failed to template application: %v", err)
	}

	objects := map[string]*unstructured.Unstructured{}
	for _, obj := range result.Objects {
		objects[obj.GetName()] = obj
	}

	reported, ok := objects["capabilities-example-app-capabilities"]
	if !ok {
		t.Fatalf("Expected the chart's ConfigMap to be rendered, got %v", objectNames(objects))
	}

	if got := configMapData(t, reported, "kubeVersion"); got != "v1.24.0" {
		t.Errorf("Expected the source's own kubeVersion to win, got %q", got)
	}

	// The API versions were not pinned by the source, so those still come from
	// the file: one field being overridden must not drop the other.
	if got := configMapData(t, reported, "hasPrometheusOperator"); got != "true" {
		t.Errorf("Expected the file's apiVersions to still apply, got %q", got)
	}
}

func TestLoadHelmCapabilities(t *testing.T) {
	caps, err := LoadHelmCapabilities("")
	if err != nil {
		t.Fatalf("Expected an empty path to be accepted: %v", err)
	}
	if caps != nil {
		t.Errorf("Expected no capabilities for an empty path, got %+v", caps)
	}

	caps, err = LoadHelmCapabilities(capabilitiesFile)
	if err != nil {
		t.Fatalf("Failed to load the example: %v", err)
	}
	if caps.KubeVersion != "v1.31.4" {
		t.Errorf("Expected kubeVersion v1.31.4, got %q", caps.KubeVersion)
	}
	if len(caps.APIVersions) != 2 || caps.APIVersions[0] != "monitoring.coreos.com/v1" {
		t.Errorf("Expected the example's apiVersions, got %v", caps.APIVersions)
	}
}

// Every one of these would otherwise fail silently: a file that is not read the
// way it was meant renders exactly like no file at all, which is a chart missing
// the resources the guards were supposed to let through.
func TestLoadHelmCapabilitiesRejectsBadFiles(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name:    "misspelled key",
			content: "apiVersion: monitoring.coreos.com/v1\n",
			wantErr: "apiVersion",
		},
		{
			name:    "not a mapping",
			content: "- monitoring.coreos.com/v1\n",
			wantErr: "failed to parse",
		},
		{
			name:    "kubeVersion that is not a version",
			content: "kubeVersion: latest\n",
			wantErr: `kubeVersion "latest"`,
		},
		{
			name:    "empty apiVersion",
			content: "apiVersions:\n  - monitoring.coreos.com/v1\n  - \"\"\n",
			wantErr: "apiVersions[1]",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "capabilities.yaml")
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatalf("Failed to write the fixture: %v", err)
			}

			_, err := LoadHelmCapabilities(path)
			if err == nil {
				t.Fatal("Expected an error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Expected the error to mention %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// A path that is not there has to be reported rather than quietly ignored, and
// the report has to reach the caller of Template rather than the render.
func TestMissingHelmCapabilitiesFileIsReported(t *testing.T) {
	_, err := Template(context.Background(), TemplateOptions{
		ApplicationFile:      "examples/directory/app.yaml",
		RepoRoot:             ".",
		HelmCapabilitiesFile: "does-not-exist.yaml",
	})
	if err == nil {
		t.Fatal("Expected an error")
	}
	if !strings.Contains(err.Error(), "HelmCapabilitiesFile") {
		t.Errorf("Expected the error to name the option, got %v", err)
	}
	if !strings.Contains(err.Error(), "does-not-exist.yaml") {
		t.Errorf("Expected the error to name the file, got %v", err)
	}
}

func objectNames(objects map[string]*unstructured.Unstructured) []string {
	names := make([]string, 0, len(objects))
	for name := range objects {
		names = append(names, name)
	}
	return names
}
