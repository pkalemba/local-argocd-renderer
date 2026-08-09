package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
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
		"namespace.yaml",
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

// The counter used to be keyed on the kind alone, so the second distinct ConfigMap
// got a "-2" suffix and overwrote one genuinely named "a-2".
func TestSaveManifestsToFilesDoesNotCollide(t *testing.T) {
	outputDir := t.TempDir()

	objects := []*unstructured.Unstructured{
		object("ConfigMap", "a-2"),
		object("ConfigMap", "a"),
		object("ConfigMap", "b"),
	}

	if err := saveManifestsToFiles(objects, outputDir, "app", true); err != nil {
		t.Fatalf("saveManifestsToFiles failed: %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(outputDir, "app"))
	if err != nil {
		t.Fatalf("Failed to read output directory: %v", err)
	}
	if len(entries) != len(objects) {
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("Expected %d files, got %d: %v", len(objects), len(entries), names)
	}

	// A distinct object keeps its own name rather than borrowing a counter.
	for _, name := range []string{"configmap-a-2.yaml", "configmap-a.yaml", "configmap-b.yaml"} {
		if _, err := os.Stat(filepath.Join(outputDir, "app", name)); err != nil {
			t.Errorf("Expected %s to be written: %v", name, err)
		}
	}
}

// --quiet used to gate only this command's own "Saved:" lines, while the Argo CD
// libraries kept logging their progress to the standard logrus logger — so the
// noisier half of the output survived, and a plain render to stdout was never
// quieted at all.
func TestQuietSilencesLibraryProgress(t *testing.T) {
	var stderr bytes.Buffer

	level := logrus.GetLevel()
	output := logrus.StandardLogger().Out
	t.Cleanup(func() {
		logrus.SetLevel(level)
		logrus.SetOutput(output)
	})
	logrus.SetOutput(&stderr)

	applyQuiet(false)
	logrus.Info("generated 2 applications")
	if !strings.Contains(stderr.String(), "generated 2 applications") {
		t.Error("Expected progress to be logged without --quiet")
	}

	stderr.Reset()
	applyQuiet(true)

	logrus.Info("generated 2 applications")
	if stderr.Len() != 0 {
		t.Errorf("Expected --quiet to suppress progress, got %q", stderr.String())
	}

	// A problem is still a problem, quiet or not.
	logrus.Warn("something looks wrong")
	if !strings.Contains(stderr.String(), "something looks wrong") {
		t.Errorf("Expected warnings to survive --quiet, got %q", stderr.String())
	}
}
