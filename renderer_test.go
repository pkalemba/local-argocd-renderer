package renderer

import (
	"context"
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/sergi/go-diff/diffmatchpatch"
)

// updateGolden rewrites the golden files instead of comparing against them, for
// when a change to the renderer is meant to change its output.
var updateGolden = flag.Bool("update", false, "rewrite the golden files")

// formatOutput renders the result exactly as the CLI does. It deliberately does no
// sorting of its own: the renderer is responsible for producing a stable order, and
// sorting here would hide it if it stopped doing so.
func formatOutput(t *testing.T, result *TemplateResult) string {
	t.Helper()

	var output strings.Builder
	if err := WriteManifests(&output, result); err != nil {
		t.Fatalf("Failed to write manifests: %v", err)
	}

	return output.String()
}

func TestTemplateFromApplication(t *testing.T) {
	ctx := context.Background()
	opts := TemplateOptions{
		ApplicationFile: "examples/directory/app.yaml",
		RepoRoot:        ".",
	}

	result, err := TemplateFromApplication(ctx, opts)
	if err != nil {
		t.Fatalf("TemplateFromApplication failed: %v", err)
	}

	if len(result.Objects) == 0 {
		t.Error("Expected at least one object")
	}

	if result.SourcesProcessed != 1 {
		t.Errorf("Expected 1 source processed, got %d", result.SourcesProcessed)
	}
}

func TestTemplateFromApplicationYAML(t *testing.T) {
	yamlContent := `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: test-app
spec:
  project: default
  source:
    repoURL: https://github.com/argoproj/argo-cd
    path: examples/directory/input
    targetRevision: HEAD
  destination:
    server: https://kubernetes.default.svc
    namespace: default
`

	ctx := context.Background()
	result, err := TemplateFromApplicationYAML(ctx, yamlContent, TemplateOptions{RepoRoot: "."})
	if err != nil {
		t.Fatalf("TemplateFromApplicationYAML failed: %v", err)
	}

	if len(result.Objects) == 0 {
		t.Error("Expected at least one object")
	}
}

type goldenTestCase struct {
	name             string
	appPath          string
	expectedPath     string
	clustersPath     string
	capabilitiesPath string
}

func TestGoldenExamples(t *testing.T) {
	testCases := []goldenTestCase{
		{
			name:         "helm",
			appPath:      "examples/helm/app.yaml",
			expectedPath: "examples/helm/expected.yaml",
		},
		{
			name:         "helm-online-kustomize",
			appPath:      "examples/helm-online-kustomize/app.yaml",
			expectedPath: "examples/helm-online-kustomize/expected.yaml",
		},
		{
			name:         "kustomize",
			appPath:      "examples/kustomize/app.yaml",
			expectedPath: "examples/kustomize/expected.yaml",
		},
		{
			name:         "directory",
			appPath:      "examples/directory/app.yaml",
			expectedPath: "examples/directory/expected.yaml",
		},
		{
			name:         "appset-list",
			appPath:      "examples/appset-list/appset.yaml",
			expectedPath: "examples/appset-list/expected.yaml",
		},
		{
			name:         "appset-git",
			appPath:      "examples/appset-git/appset.yaml",
			expectedPath: "examples/appset-git/expected.yaml",
		},
		{
			name:         "appset-clusters",
			appPath:      "examples/appset-clusters/appset.yaml",
			expectedPath: "examples/appset-clusters/expected.yaml",
			clustersPath: "examples/appset-clusters/clusters.yaml",
		},
		{
			name:         "appset-clusters-helm",
			appPath:      "examples/appset-clusters-helm/appset.yaml",
			expectedPath: "examples/appset-clusters-helm/expected.yaml",
			clustersPath: "examples/appset-clusters-helm/clusters.yaml",
		},
		{
			name:             "helm-capabilities",
			appPath:          "examples/helm-capabilities/app.yaml",
			expectedPath:     "examples/helm-capabilities/expected.yaml",
			capabilitiesPath: "examples/helm-capabilities/capabilities.yaml",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Call the library function directly
			ctx := context.Background()
			opts := TemplateOptions{
				ApplicationFile:      tc.appPath,
				RepoRoot:             ".",
				ClustersFile:         tc.clustersPath,
				HelmCapabilitiesFile: tc.capabilitiesPath,
			}

			result, err := Template(ctx, opts)
			if err != nil {
				t.Fatalf("Failed to template application: %v", err)
			}

			output := formatOutput(t, result)

			if *updateGolden {
				if err := os.WriteFile(tc.expectedPath, []byte(output), 0o644); err != nil {
					t.Fatalf("Failed to write %s: %v", tc.expectedPath, err)
				}
				t.Logf("Wrote golden file: %s", tc.expectedPath)
				return
			}

			// A missing golden used to be written out and the test passed, so a
			// deleted golden — or a new case whose golden was never committed —
			// could never fail. Only -update writes them now.
			expectedBytes, err := os.ReadFile(tc.expectedPath)
			if err != nil {
				t.Fatalf("Failed to read %s (run `go test -update` to create it): %v", tc.expectedPath, err)
			}
			expected := string(expectedBytes)

			if strings.TrimSpace(output) != strings.TrimSpace(expected) {
				t.Errorf("Output does not match %s", tc.expectedPath)

				dmp := diffmatchpatch.New()
				diffs := dmp.DiffMain(expected, output, false)
				t.Errorf("Diff:\n%s", dmp.DiffPrettyText(diffs))
			}
		})
	}
}
