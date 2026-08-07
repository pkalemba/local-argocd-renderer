package main

import (
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func object(kind, name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("v1")
	obj.SetKind(kind)
	if name != "" {
		obj.SetName(name)
	}

	return obj
}

func TestSaveManifestsToFiles(t *testing.T) {
	outputDir := t.TempDir()

	objects := []*unstructured.Unstructured{
		object("ConfigMap", "settings"),
		object("Service", "guestbook-ui"),
		// Same kind and name again: the second one gets a counter suffix.
		object("ConfigMap", "settings"),
		object("Namespace", ""),
	}

	if err := saveManifestsToFiles(objects, outputDir, "my-app", true); err != nil {
		t.Fatalf("saveManifestsToFiles failed: %v", err)
	}

	for _, name := range []string{
		"configmap-settings.yaml",
		"service-guestbook-ui.yaml",
		"configmap-settings-2.yaml",
		"namespace-1.yaml",
	} {
		path := filepath.Join(outputDir, "my-app", name)
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("Expected %s to be written: %v", path, err)
			continue
		}
		if len(contents) == 0 {
			t.Errorf("Expected %s to have contents", path)
		}
	}

	entries, err := os.ReadDir(filepath.Join(outputDir, "my-app"))
	if err != nil {
		t.Fatalf("Failed to read output directory: %v", err)
	}
	if len(entries) != len(objects) {
		t.Errorf("Expected %d files, got %d", len(objects), len(entries))
	}
}
