package renderer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// checkout writes an Application whose source.path is the repo-relative "input",
// alongside the manifest that path holds, into a directory that is deliberately
// nowhere near the process's working directory.
func checkout(t *testing.T, sourcePath string) string {
	t.Helper()

	root := t.TempDir()

	input := filepath.Join(root, "input")
	if err := os.MkdirAll(input, 0o755); err != nil {
		t.Fatalf("Failed to create %s: %v", input, err)
	}
	manifest := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: from-the-checkout\n"
	if err := os.WriteFile(filepath.Join(input, "configmap.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("Failed to write the manifest: %v", err)
	}

	app := `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: elsewhere
spec:
  project: default
  source:
    repoURL: https://github.com/argoproj/argo-cd
    path: ` + sourcePath + `
  destination:
    server: https://kubernetes.default.svc
    namespace: dest-ns
`
	if err := os.WriteFile(filepath.Join(root, "app.yaml"), []byte(app), 0o644); err != nil {
		t.Fatalf("Failed to write the Application: %v", err)
	}

	return root
}

// RepoRoot was passed to the repo-server but never used to resolve the source's
// path, which was read relative to the process's working directory instead. So it
// only ever worked when the caller happened to already be standing in the repo —
// rendering a checkout from anywhere else failed with an opaque
// "lstat input: no such file or directory".
func TestRepoRootResolvesTheSourcePath(t *testing.T) {
	root := checkout(t, "input")

	result, err := Template(context.Background(), TemplateOptions{
		ApplicationFile: filepath.Join(root, "app.yaml"),
		RepoRoot:        root,
	})
	if err != nil {
		t.Fatalf("Template failed: %v", err)
	}

	if len(result.Objects) != 1 || result.Objects[0].GetName() != "from-the-checkout" {
		t.Fatalf("Expected the checkout's ConfigMap, got %d objects", len(result.Objects))
	}
	if got := result.Objects[0].GetNamespace(); got != "dest-ns" {
		t.Errorf("Expected the object in dest-ns, got %q", got)
	}
}

// argopath.Path is the repo-server's own resolver, so its guard rails come along:
// a source may not point outside the checkout, or at a file, or at nothing.
func TestSourcePathsOutsideTheCheckoutAreRejected(t *testing.T) {
	for _, tc := range []struct {
		name       string
		sourcePath string
		contains   string
	}{
		{name: "escaping the root", sourcePath: "../..", contains: "app path outside root"},
		{name: "absolute", sourcePath: "/etc", contains: "app path is absolute"},
		{name: "missing", sourcePath: "nowhere", contains: "app path does not exist"},
		{name: "a file", sourcePath: "app.yaml", contains: "app path is not a directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := checkout(t, tc.sourcePath)

			_, err := Template(context.Background(), TemplateOptions{
				ApplicationFile: filepath.Join(root, "app.yaml"),
				RepoRoot:        root,
			})
			if err == nil {
				t.Fatal("Expected an error")
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Errorf("Expected the error to mention %q, got %v", tc.contains, err)
			}
		})
	}
}

// The default stays what it was: the working directory.
func TestRepoRootDefaultsToTheWorkingDirectory(t *testing.T) {
	result, err := Template(context.Background(), TemplateOptions{
		ApplicationFile: "examples/directory/app.yaml",
	})
	if err != nil {
		t.Fatalf("Template failed: %v", err)
	}
	if len(result.Objects) == 0 {
		t.Error("Expected the example to render with no RepoRoot set")
	}
}
