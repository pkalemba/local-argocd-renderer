package renderer

import (
	"context"
	"strings"
	"testing"
)

// A caller's typo used to reach resource.MustParse, which takes the process down
// with a panic from inside a Kubernetes helper instead of reporting the bad value.
func TestMaxManifestSizeRejectsBadValues(t *testing.T) {
	size, err := maxManifestSize("")
	if err != nil {
		t.Fatalf("Expected the default to parse: %v", err)
	}
	if got := size.String(); got != defaultMaxManifestSize {
		t.Errorf("Expected the repo-server's default %s, got %s", defaultMaxManifestSize, got)
	}

	size, err = maxManifestSize("32Mi")
	if err != nil {
		t.Fatalf("Expected 32Mi to parse: %v", err)
	}
	if got := size.String(); got != "32Mi" {
		t.Errorf("Expected 32Mi, got %s", got)
	}

	_, err = maxManifestSize("10 megabytes")
	if err == nil {
		t.Fatal("Expected an error rather than a panic")
	}
	if !strings.Contains(err.Error(), "10 megabytes") {
		t.Errorf("Expected the error to quote the offending value, got %v", err)
	}
}

// The bad value has to surface as an error from the render rather than unwinding
// the caller's stack.
func TestBadMaxManifestSizeIsReturnedNotPanicked(t *testing.T) {
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("Expected an error, got a panic: %v", recovered)
		}
	}()

	_, err := Template(context.Background(), TemplateOptions{
		ApplicationFile: "examples/directory/app.yaml",
		RepoRoot:        ".",
		MaxManifestSize: "not a size",
	})
	if err == nil {
		t.Fatal("Expected an error")
	}
	if !strings.Contains(err.Error(), "MaxManifestSize") {
		t.Errorf("Expected the error to name the option, got %v", err)
	}
}

// The ...YAML entry points took a bare repoRoot and built the options themselves,
// so everything else the caller asked for was silently dropped.
func TestTemplateFromApplicationYAMLKeepsItsOptions(t *testing.T) {
	manifest, err := readManifest("examples/helm/app.yaml")
	if err != nil {
		t.Fatalf("Failed to read the example: %v", err)
	}

	// MaxManifestSize is the cheapest option to observe: it is only consulted if
	// the options actually reach the render.
	_, err = TemplateFromApplicationYAML(context.Background(), string(manifest), TemplateOptions{
		RepoRoot:        ".",
		MaxManifestSize: "not a size",
	})
	if err == nil || !strings.Contains(err.Error(), "MaxManifestSize") {
		t.Errorf("Expected the options to reach the render, got %v", err)
	}
}

func TestTemplateFromApplicationSetYAMLKeepsItsOptions(t *testing.T) {
	manifest, err := readManifest("examples/appset-list/appset.yaml")
	if err != nil {
		t.Fatalf("Failed to read the example: %v", err)
	}

	_, err = TemplateFromApplicationSetYAML(context.Background(), string(manifest), TemplateOptions{
		RepoRoot:        ".",
		MaxManifestSize: "not a size",
	})
	if err == nil || !strings.Contains(err.Error(), "MaxManifestSize") {
		t.Errorf("Expected the options to reach the render, got %v", err)
	}
}
