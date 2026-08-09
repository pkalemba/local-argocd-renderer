package renderer

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

// The chart reference used to be built as "<repoURL>/<chart>", which helm reads as
// a literal archive URL rather than a repository to resolve the chart in.
func TestHelmPullArgs(t *testing.T) {
	for _, tc := range []struct {
		name     string
		repoURL  string
		chart    string
		version  string
		expected []string
	}{
		{
			name:     "http repository is resolved through its index",
			repoURL:  "https://charts.example.com",
			chart:    "nginx",
			version:  "0.1.6",
			expected: []string{"pull", "nginx", "--repo", "https://charts.example.com", "--version", "0.1.6", "--destination", "/cache", "--untar"},
		},
		{
			// Argo CD allows an Application to omit targetRevision, in which case
			// helm picks the newest version in the index.
			name:     "version is optional",
			repoURL:  "https://charts.example.com",
			chart:    "nginx",
			expected: []string{"pull", "nginx", "--repo", "https://charts.example.com", "--destination", "/cache", "--untar"},
		},
		{
			// A registry has no index, so the reference is the address itself and
			// helm rejects --repo alongside it.
			name:     "oci reference is passed whole",
			repoURL:  "oci://registry-1.docker.io/cloudpirates",
			chart:    "nginx",
			version:  "0.1.6",
			expected: []string{"pull", "oci://registry-1.docker.io/cloudpirates/nginx", "--version", "0.1.6", "--destination", "/cache", "--untar"},
		},
		{
			name:     "oci reference does not gain a double slash",
			repoURL:  "oci://registry-1.docker.io/cloudpirates/",
			chart:    "nginx",
			expected: []string{"pull", "oci://registry-1.docker.io/cloudpirates/nginx", "--destination", "/cache", "--untar"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := helmPullArgs(tc.repoURL, tc.chart, tc.version, "/cache")
			if !slices.Equal(args, tc.expected) {
				t.Errorf("Expected\n  %v\ngot\n  %v", tc.expected, args)
			}
		})
	}
}

// An end-to-end pull against a chart repository served from disk. It needs no
// outside network, and it fails on the old invocation because there is nothing
// to download at <repoURL>/<chart>.
func TestDownloadHelmChartFromRepository(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}

	repoDir := t.TempDir()
	server := httptest.NewServer(http.FileServer(http.Dir(repoDir)))
	t.Cleanup(server.Close)

	chartDir := filepath.Join(t.TempDir(), "hello")
	if err := os.MkdirAll(filepath.Join(chartDir, "templates"), 0o755); err != nil {
		t.Fatalf("Failed to create the chart: %v", err)
	}
	write := func(name, contents string) {
		if err := os.WriteFile(filepath.Join(chartDir, name), []byte(contents), 0o644); err != nil {
			t.Fatalf("Failed to write %s: %v", name, err)
		}
	}
	write("Chart.yaml", "apiVersion: v2\nname: hello\nversion: 0.1.0\n")
	write("templates/configmap.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: hello\n")

	run := func(args ...string) {
		t.Helper()
		if output, err := exec.Command("helm", args...).CombinedOutput(); err != nil {
			t.Fatalf("helm %v failed: %v\n%s", args, err, output)
		}
	}
	run("package", chartDir, "--destination", repoDir)
	run("repo", "index", repoDir, "--url", server.URL)

	// downloadHelmChart caches under XDG_CACHE_HOME; keep that out of the
	// developer's real cache so the test starts from a cold one every time.
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	pulled, err := downloadHelmChart(server.URL, "hello", "0.1.0")
	if err != nil {
		t.Fatalf("downloadHelmChart failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(pulled, "Chart.yaml")); err != nil {
		t.Errorf("Expected the pulled chart at %s: %v", pulled, err)
	}

	// The second call has to come back from the cache rather than pulling again.
	cached, err := downloadHelmChart(server.URL, "hello", "0.1.0")
	if err != nil {
		t.Fatalf("downloadHelmChart failed on a cached chart: %v", err)
	}
	if cached != pulled {
		t.Errorf("Expected the cached chart at %s, got %s", pulled, cached)
	}
}
