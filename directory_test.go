package renderer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const directoryApp = `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: dir-app
spec:
  project: default
  source:
    repoURL: https://github.com/argoproj/argo-cd
    path: examples/appset-list/input/one
  destination:
    server: https://kubernetes.default.svc
    namespace: one-namespace
`

// writeTree lays out files below a fresh directory, creating parents as needed.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()

	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("Failed to create %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("Failed to write %s: %v", path, err)
		}
	}

	return root
}

func TestTemplateFromDirectory(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"apps/app.yaml":          directoryApp,
		"apps/appset.yaml":       listAppSet,
		"unrelated.yaml":         "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: skipped\n",
		"notes.md":               "not a manifest",
		".hidden/app.yaml":       directoryApp,
		"apps/README.txt":        "also not a manifest",
		"apps/empty.yaml":        "# just a comment\n",
		"apps/kustomization.yml": "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n",
	})

	result, err := Template(context.Background(), TemplateOptions{
		ApplicationDir: dir,
		RepoRoot:       ".",
	})
	if err != nil {
		t.Fatalf("Template failed: %v", err)
	}

	// One Application from app.yaml plus the two the ApplicationSet generates. The
	// ConfigMap, the Kustomization, the non-manifest files and the hidden directory
	// are all skipped.
	if result.ApplicationsProcessed != 3 {
		t.Errorf("Expected 3 applications, got %d", result.ApplicationsProcessed)
	}

	names := map[string]bool{}
	for _, app := range result.Applications {
		names[app.Name] = true
	}
	for _, expected := range []string{"dir-app", "test-one", "test-two"} {
		if !names[expected] {
			t.Errorf("Expected an application named %q, got %v", expected, names)
		}
	}

	for _, obj := range result.Objects {
		if obj.GetName() == "skipped" {
			t.Error("Expected the ConfigMap outside any Application to be skipped")
		}
	}
}

func TestTemplateFromDirectoryIsOrdered(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"c.yaml": strings.Replace(directoryApp, "dir-app", "c-app", 1),
		"a.yaml": strings.Replace(directoryApp, "dir-app", "a-app", 1),
		"b.yaml": strings.Replace(directoryApp, "dir-app", "b-app", 1),
	})

	for range 3 {
		result, err := Template(context.Background(), TemplateOptions{ApplicationDir: dir, RepoRoot: "."})
		if err != nil {
			t.Fatalf("Template failed: %v", err)
		}

		var order []string
		for _, app := range result.Applications {
			order = append(order, app.Name)
		}
		if strings.Join(order, ",") != "a-app,b-app,c-app" {
			t.Fatalf("Expected the files to be rendered in sorted order, got %v", order)
		}
	}
}

func TestTemplateFromMultiDocumentFile(t *testing.T) {
	second := strings.Replace(directoryApp, "dir-app", "second-app", 1)

	result, err := TemplateFromYAML(context.Background(), directoryApp+"\n---\n"+second, TemplateOptions{RepoRoot: "."})
	if err != nil {
		t.Fatalf("TemplateFromYAML failed: %v", err)
	}

	if result.ApplicationsProcessed != 2 {
		t.Fatalf("Expected both documents to be rendered, got %d applications", result.ApplicationsProcessed)
	}
}

func TestTemplateRejectsFileAndDirTogether(t *testing.T) {
	_, err := Template(context.Background(), TemplateOptions{
		ApplicationFile: "examples/directory/app.yaml",
		ApplicationDir:  "examples",
		RepoRoot:        ".",
	})
	if err == nil {
		t.Fatal("Expected an error when both a file and a directory are given")
	}
}

func TestIncludeApplications(t *testing.T) {
	ctx := context.Background()
	opts := TemplateOptions{RepoRoot: ".", IncludeApplications: true}

	result, err := templateDocuments(ctx, []byte(listAppSet), opts, false)
	if err != nil {
		t.Fatalf("Failed to render: %v", err)
	}

	if len(result.Applications) != 2 {
		t.Fatalf("Expected 2 applications, got %d", len(result.Applications))
	}

	for _, app := range result.Applications {
		if len(app.Objects) != 2 {
			t.Fatalf("Expected the Application to be emitted next to its manifest, got %d objects", len(app.Objects))
		}

		// The Application comes first, ahead of what it renders to.
		manifest := app.Objects[0]
		if manifest.GetKind() != "Application" || manifest.GetAPIVersion() != "argoproj.io/v1alpha1" {
			t.Errorf("Expected an argoproj.io/v1alpha1 Application, got %s %s", manifest.GetAPIVersion(), manifest.GetKind())
		}
		if manifest.GetName() != app.Name {
			t.Errorf("Expected the Application to be named %q, got %q", app.Name, manifest.GetName())
		}
		if _, found := manifest.Object["status"]; found {
			t.Error("Expected the empty status to be dropped")
		}
		if _, found := manifest.Object["metadata"].(map[string]any)["creationTimestamp"]; found {
			t.Error("Expected the empty creationTimestamp to be dropped")
		}
	}
}

func TestIncludeApplicationsIsOptOut(t *testing.T) {
	result, err := templateDocuments(context.Background(), []byte(listAppSet), TemplateOptions{RepoRoot: "."}, false)
	if err != nil {
		t.Fatalf("Failed to render: %v", err)
	}

	for _, obj := range result.Objects {
		if obj.GetKind() == "Application" {
			t.Error("Expected no Application resources without IncludeApplications")
		}
	}
}
