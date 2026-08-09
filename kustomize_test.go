package renderer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The overlay used to be created with os.MkdirTemp(".", ...), which put it inside
// the repository being rendered. That fails outright when the working directory is
// read-only — the shipped container image runs as nobody with the repo mounted at
// WORKDIR — and litters the checkout when a render is interrupted.
func TestKustomizeOverlayIsOutsideTheWorkingDirectory(t *testing.T) {
	overlay, err := kustomizeOverlay("examples/kustomize/input")
	if err != nil {
		t.Fatalf("kustomizeOverlay failed: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(overlay) })

	if !filepath.IsAbs(overlay) {
		t.Errorf("Expected an absolute path, got %q", overlay)
	}

	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Failed to read the working directory: %v", err)
	}
	if strings.HasPrefix(overlay, workingDir+string(os.PathSeparator)) {
		t.Errorf("Expected the overlay outside %s, got %q", workingDir, overlay)
	}

	// Kustomize rejects an absolute resource path outright ("new root ... cannot be
	// absolute"), so the reference back into the checkout has to be relative.
	kustomization, err := os.ReadFile(filepath.Join(overlay, "kustomization.yaml"))
	if err != nil {
		t.Fatalf("Failed to read the generated kustomization: %v", err)
	}
	for _, line := range strings.Split(string(kustomization), "\n") {
		resource, found := strings.CutPrefix(line, "- ")
		if found && filepath.IsAbs(resource) {
			t.Errorf("Expected a relative resource path, got %q", resource)
		}
	}
}

// The overlay is only useful if kustomize can actually resolve it back to the
// source, which is the part that breaks if the relative path is computed wrong.
func TestKustomizeOverlayResolves(t *testing.T) {
	if _, err := exec.LookPath("kustomize"); err != nil {
		t.Skip("kustomize is not installed")
	}

	overlay, err := kustomizeOverlay("examples/kustomize/input")
	if err != nil {
		t.Fatalf("kustomizeOverlay failed: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(overlay) })

	output, err := exec.Command("kustomize", "build", overlay).CombinedOutput()
	if err != nil {
		t.Fatalf("kustomize could not build the overlay: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "kind: Deployment") {
		t.Errorf("Expected the source's manifests, got:\n%s", output)
	}
}

// Rendering must leave the checkout exactly as it found it.
func TestRenderingLeavesNoResidue(t *testing.T) {
	if _, err := exec.LookPath("kustomize"); err != nil {
		t.Skip("kustomize is not installed")
	}

	before, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("Failed to list the working directory: %v", err)
	}

	if _, err := Template(context.Background(), TemplateOptions{
		ApplicationFile: "examples/kustomize/app.yaml",
		RepoRoot:        ".",
	}); err != nil {
		t.Fatalf("Template failed: %v", err)
	}

	after, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("Failed to list the working directory: %v", err)
	}

	if len(after) != len(before) {
		var added []string
		existing := map[string]bool{}
		for _, entry := range before {
			existing[entry.Name()] = true
		}
		for _, entry := range after {
			if !existing[entry.Name()] {
				added = append(added, entry.Name())
			}
		}
		t.Errorf("Rendering left %v behind in the working directory", added)
	}
}
