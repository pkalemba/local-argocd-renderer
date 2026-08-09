package renderer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// manyObjects is an Application over a directory holding enough objects that a
// map-ordered result is overwhelmingly unlikely to come back the same twice.
func manyObjects(t *testing.T) TemplateOptions {
	t.Helper()

	root := t.TempDir()

	input := filepath.Join(root, "input")
	if err := os.MkdirAll(input, 0o755); err != nil {
		t.Fatalf("Failed to create %s: %v", input, err)
	}

	for _, name := range []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel"} {
		manifest := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + name + "\ndata:\n  key: value\n"
		if err := os.WriteFile(filepath.Join(input, name+".yaml"), []byte(manifest), 0o644); err != nil {
			t.Fatalf("Failed to write %s: %v", name, err)
		}
	}

	app := `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: ordered-app
spec:
  project: default
  source:
    repoURL: https://github.com/argoproj/argo-cd
    path: PATHPLACEHOLDER
  destination:
    server: https://kubernetes.default.svc
    namespace: default
`
	app = strings.Replace(app, "PATHPLACEHOLDER", "input", 1)

	appPath := filepath.Join(root, "app.yaml")
	if err := os.WriteFile(appPath, []byte(app), 0o644); err != nil {
		t.Fatalf("Failed to write %s: %v", appPath, err)
	}

	return TemplateOptions{ApplicationFile: appPath, RepoRoot: root}
}

// The renderer exists so that its output can be diffed. Deduplication collects its
// result by ranging over a map, so without an explicit sort the order changed on
// every run and every diff was noise.
func TestRenderIsDeterministic(t *testing.T) {
	opts := manyObjects(t)

	var first string
	for run := range 8 {
		result, err := Template(context.Background(), opts)
		if err != nil {
			t.Fatalf("Template failed: %v", err)
		}

		output := formatOutput(t, result)
		if run == 0 {
			first = output
			continue
		}
		if output != first {
			t.Fatalf("Run %d produced different output than run 0:\n--- run 0 ---\n%s\n--- run %d ---\n%s", run, first, run, output)
		}
	}
}

func TestRenderIsSorted(t *testing.T) {
	result, err := Template(context.Background(), manyObjects(t))
	if err != nil {
		t.Fatalf("Template failed: %v", err)
	}

	var names []string
	for _, obj := range result.Objects {
		names = append(names, obj.GetName())
	}

	want := "alpha,bravo,charlie,delta,echo,foxtrot,golf,hotel"
	if got := strings.Join(names, ","); got != want {
		t.Errorf("Expected the objects in sorted order\n got: %s\nwant: %s", got, want)
	}
}

// Distinct objects must never share a filename. Keying the counter on the kind
// alone gave the second ConfigMap a "-2" suffix, which overwrote a ConfigMap
// genuinely named "...-2".
func TestSortIsStableAcrossKinds(t *testing.T) {
	result, err := Template(context.Background(), manyObjects(t))
	if err != nil {
		t.Fatalf("Template failed: %v", err)
	}

	// Sorting must not lose or duplicate anything.
	if len(result.Objects) != 8 {
		t.Fatalf("Expected 8 objects, got %d", len(result.Objects))
	}
	seen := map[string]bool{}
	for _, obj := range result.Objects {
		key := obj.GetKind() + "/" + obj.GetName()
		if seen[key] {
			t.Errorf("Object %s appears twice", key)
		}
		seen[key] = true
	}
}
