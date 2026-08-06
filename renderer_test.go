package renderer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/sergi/go-diff/diffmatchpatch"
)

// formatOutput formats the TemplateResult the same way as the CLI
func formatOutput(result *TemplateResult) string {
	var output strings.Builder

	// Header line
	fmt.Fprintf(&output, "# Generated %d manifests from %d sources (%d after deduplication)\n", len(result.Objects), result.SourcesProcessed, len(result.Objects))
	if result.ApplicationsProcessed > 1 {
		fmt.Fprintf(&output, "# Rendered %d applications\n", result.ApplicationsProcessed)
	}
	output.WriteString("---\n")

	// Sort manifests for consistent ordering (by kind, then by name)
	sortedObjects := make([]*unstructured.Unstructured, len(result.Objects))
	copy(sortedObjects, result.Objects)

	// Simple sort by kind, then by name for consistency
	for i := 0; i < len(sortedObjects)-1; i++ {
		for j := i + 1; j < len(sortedObjects); j++ {
			kind1 := sortedObjects[i].GetKind()
			kind2 := sortedObjects[j].GetKind()
			name1 := sortedObjects[i].GetName()
			name2 := sortedObjects[j].GetName()

			// Sort by kind first, then by name
			if kind1 > kind2 || (kind1 == kind2 && name1 > name2) {
				sortedObjects[i], sortedObjects[j] = sortedObjects[j], sortedObjects[i]
			}
		}
	}

	// Manifests
	for i, obj := range sortedObjects {
		if i > 0 {
			output.WriteString("---\n")
		}

		yamlBytes, err := yaml.Marshal(obj.Object)
		if err != nil {
			// If YAML conversion fails, output JSON
			jsonBytes, _ := json.Marshal(obj.Object)
			output.WriteString(string(jsonBytes))
			continue
		}

		output.WriteString(string(yamlBytes))
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
	result, err := TemplateFromApplicationYAML(ctx, yamlContent, ".")
	if err != nil {
		t.Fatalf("TemplateFromApplicationYAML failed: %v", err)
	}

	if len(result.Objects) == 0 {
		t.Error("Expected at least one object")
	}
}

type goldenTestCase struct {
	name         string
	appPath      string
	expectedPath string
	clustersPath string
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
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Call the library function directly
			ctx := context.Background()
			opts := TemplateOptions{
				ApplicationFile: tc.appPath,
				RepoRoot:        ".",
				ClustersFile:    tc.clustersPath,
			}

			result, err := Template(ctx, opts)
			if err != nil {
				t.Fatalf("Failed to template application: %v", err)
			}

			// Format output the same way as CLI
			output := formatOutput(result)

			// Check if expected output exists
			if _, err := os.Stat(tc.expectedPath); os.IsNotExist(err) {
				// Write the output as the expected golden file
				if err := os.WriteFile(tc.expectedPath, []byte(output), 0644); err != nil {
					t.Fatalf("Failed to write expected output: %v", err)
				}
				t.Logf("Created golden file: %s", tc.expectedPath)
			} else {
				// Read the expected output
				expectedBytes, err := os.ReadFile(tc.expectedPath)
				if err != nil {
					t.Fatalf("Failed to read expected output: %v", err)
				}
				expected := string(expectedBytes)

				// Compare outputs
				if strings.TrimSpace(output) != strings.TrimSpace(expected) {
					t.Errorf("Output does not match expected golden file")

					dmp := diffmatchpatch.New()
					diffs := dmp.DiffMain(expected, output, false)
					diffText := dmp.DiffPrettyText(diffs)
					t.Errorf("Diff:\n%s", diffText)
				}
			}
		})
	}
}
