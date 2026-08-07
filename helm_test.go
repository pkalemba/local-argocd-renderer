package renderer

import (
	"context"
	"os/exec"
	"testing"
)

// testHookName is the Pod the example chart ships behind a helm.sh/hook: test
// annotation, which `helm template` renders like any other manifest.
const testHookName = "helm-example-app-test-connection"

func renderedNames(t *testing.T, opts TemplateOptions) map[string]bool {
	t.Helper()

	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}

	result, err := Template(context.Background(), opts)
	if err != nil {
		t.Fatalf("Failed to template application: %v", err)
	}

	names := map[string]bool{}
	for _, obj := range result.Objects {
		names[obj.GetName()] = true
	}

	return names
}

// The default has to match what Argo CD renders, test hooks included.
func TestHelmTestsAreRenderedByDefault(t *testing.T) {
	names := renderedNames(t, TemplateOptions{
		ApplicationFile: "examples/helm/app.yaml",
		RepoRoot:        ".",
	})

	if !names[testHookName] {
		t.Errorf("Expected the chart's test hook to be rendered, got %v", names)
	}
}

func TestSkipHelmTests(t *testing.T) {
	names := renderedNames(t, TemplateOptions{
		ApplicationFile: "examples/helm/app.yaml",
		RepoRoot:        ".",
		SkipHelmTests:   true,
	})

	if names[testHookName] {
		t.Error("Expected the chart's test hook to be skipped")
	}

	// The rest of the chart still has to come through.
	for _, expected := range []string{"helm-example-config", "helm-example-service", "helm-example-deployment"} {
		if !names[expected] {
			t.Errorf("Expected %q to still be rendered, got %v", expected, names)
		}
	}
}

// A non-zero helm block is what marks a source as Helm, so setting SkipTests on
// a source that is not one would retype it and change what gets rendered.
func TestSkipHelmTestsLeavesOtherSourceTypesAlone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		appPath string
		tool    string
		object  string
	}{
		{name: "kustomize", appPath: "examples/kustomize/app.yaml", tool: "kustomize", object: "staging-my-app-v2"},
		{name: "directory", appPath: "examples/directory/app.yaml", object: "guestbook-ui"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.tool != "" {
				if _, err := exec.LookPath(tc.tool); err != nil {
					t.Skipf("%s is not installed", tc.tool)
				}
			}

			opts := TemplateOptions{ApplicationFile: tc.appPath, RepoRoot: ".", SkipHelmTests: true}
			result, err := Template(context.Background(), opts)
			if err != nil {
				t.Fatalf("Failed to template application: %v", err)
			}

			var found bool
			for _, obj := range result.Objects {
				if obj.GetName() == tc.object {
					found = true
				}
			}
			if !found {
				t.Errorf("Expected %q to be rendered unchanged with SkipHelmTests set", tc.object)
			}
		})
	}
}
