package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestIsReleaseVersion(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    bool
		why     string
	}{
		{version: "v1.0.0", want: true},
		{version: "v1.11.2", want: true},
		{version: "v10.0.1", want: true},

		{version: "dev", want: false, why: "the default when no ldflags are set"},
		// `git describe` on a commit past the tag. semver parses it, but as a
		// prerelease of v1.1.0 — which sorts BELOW v1.1.0, so updating would move
		// the binary backwards to the tag it was built from.
		{version: "v1.1.0-3-gabc1234", want: false, why: "git describe past a tag"},
		{version: "v1.1.0-dirty", want: false, why: "uncommitted changes"},
		{version: "abc1234", want: false, why: "git describe --always with no tags"},
		{version: "1.0.0", want: false, why: "our tags carry the v prefix"},
		{version: "", want: false},
	} {
		if got := isReleaseVersion(tc.version); got != tc.want {
			t.Errorf("isReleaseVersion(%q) = %v, want %v (%s)", tc.version, got, tc.want, tc.why)
		}
	}
}

// A build that is not a release must be refused before any network call.
func TestSelfUpdateRefusesNonReleaseBuilds(t *testing.T) {
	for _, version := range []string{"dev", "v1.1.0-3-gabc1234", "abc1234"} {
		err := selfUpdate(t.Context(), version, true)
		if err == nil {
			t.Errorf("Expected %q to be refused", version)
		}
	}
}

func TestReplaceableModeReportsThePermissions(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "local-argocd-renderer")
	if err := os.WriteFile(executable, []byte("binary"), 0o700); err != nil {
		t.Fatalf("Failed to write %s: %v", executable, err)
	}

	mode, err := replaceableMode(executable)
	if err != nil {
		t.Fatalf("replaceableMode failed: %v", err)
	}
	if mode != 0o700 {
		t.Errorf("Expected the mode to be reported as 0700 so it can be restored, got %o", mode)
	}
}

func TestReplaceableModeRejectsPackageManagerPaths(t *testing.T) {
	for _, executable := range []string{
		"/nix/store/abc123-local-argocd-renderer/bin/local-argocd-renderer",
		"/opt/homebrew/Cellar/local-argocd-renderer/1.0.0/bin/local-argocd-renderer",
	} {
		if _, err := replaceableMode(executable); err == nil {
			t.Errorf("Expected %s to be refused as package-manager owned", executable)
		}
	}
}

func TestReplaceableModeRejectsUnwritableDirectories(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not work the same way on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root can write to a read-only directory")
	}

	directory := t.TempDir()
	executable := filepath.Join(directory, "local-argocd-renderer")
	if err := os.WriteFile(executable, []byte("binary"), 0o755); err != nil {
		t.Fatalf("Failed to write %s: %v", executable, err)
	}
	if err := os.Chmod(directory, 0o500); err != nil {
		t.Fatalf("Failed to make %s read-only: %v", directory, err)
	}
	t.Cleanup(func() { _ = os.Chmod(directory, 0o700) })

	if _, err := replaceableMode(executable); err == nil {
		t.Error("Expected an unwritable directory to be refused before downloading anything")
	}
}
