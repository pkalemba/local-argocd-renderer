package renderer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// A multi-source Application can keep its chart in one source and the values it
// is rendered with in another, naming the second source's `ref` as $name from the
// first source's valueFiles. Everything below renders that shape.

// refRepo lays out a checkout holding a chart and, away from it, the value files
// a sibling source hands to it:
//
//	<root>/chart/...          the chart, with its own defaults
//	<root>/values/prod.yaml   the values the ref source provides
//	<root>/stray.yaml         a manifest that belongs to neither
//
// stray.yaml sits at the checkout root deliberately: a ref source has no path of
// its own, and an empty path resolves to exactly that root.
func refRepo(t *testing.T) string {
	t.Helper()

	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}

	root := t.TempDir()

	write := func(path, content string) {
		t.Helper()

		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("Failed to create %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("Failed to write %s: %v", full, err)
		}
	}

	write("chart/Chart.yaml", "apiVersion: v2\nname: ref-chart\nversion: 0.1.0\n")
	write("chart/values.yaml", "message: chart default\nreplicaCount: 1\n")
	write("chart/templates/configmap.yaml", `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}-config
data:
  message: {{ .Values.message | quote }}
  replicas: {{ .Values.replicaCount | quote }}
`)
	write("values/prod.yaml", "message: from the ref source\nreplicaCount: 3\n")
	write("values/extra.yaml", "replicaCount: 9\n")
	write("shared/configmap.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: shared\n")
	write("stray.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: stray\n")

	return root
}

// refApp is a two-source Application: a ref source that renders nothing, and a
// chart that reads valueFiles out of it. valueFiles is spliced in verbatim so a
// test can hand it a path that does not resolve.
func refApp(valueFiles ...string) string {
	app := `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: ref-app
spec:
  project: default
  sources:
  - repoURL: https://github.com/myorg/myrepo
    targetRevision: HEAD
    ref: values
  - repoURL: https://github.com/myorg/myrepo
    path: chart
    targetRevision: HEAD
    helm:
      valueFiles:
`
	for _, valueFile := range valueFiles {
		app += "      - " + valueFile + "\n"
	}

	return app + `  destination:
    server: https://kubernetes.default.svc
    namespace: ref-namespace
`
}

// renderRefApp renders app against root and returns the ConfigMap data the chart
// templated, which is what the values that reached helm can be read off.
func renderRefApp(t *testing.T, root, app string) (*TemplateResult, map[string]string) {
	t.Helper()

	result, err := TemplateFromApplicationYAML(context.Background(), app, TemplateOptions{RepoRoot: root})
	if err != nil {
		t.Fatalf("Failed to template application: %v", err)
	}

	for _, obj := range result.Objects {
		if obj.GetName() != "ref-app-config" {
			continue
		}

		data, found, err := unstructured.NestedStringMap(obj.Object, "data")
		if err != nil || !found {
			t.Fatalf("Expected the rendered ConfigMap to carry data, got %v (err %v)", obj.Object, err)
		}

		return result, data
	}

	t.Fatalf("Expected the chart source to render ref-app-config, got %v", renderedObjectNames(result))

	return nil, nil
}

func renderedObjectNames(result *TemplateResult) []string {
	names := make([]string, 0, len(result.Objects))
	for _, obj := range result.Objects {
		names = append(names, obj.GetName())
	}

	return names
}

// The values the ref source holds have to win over the chart's own values.yaml —
// that is the whole point of pointing the chart at them.
func TestRefSourceValuesReachTheChart(t *testing.T) {
	root := refRepo(t)

	_, data := renderRefApp(t, root, refApp("$values/values/prod.yaml"))

	for key, want := range map[string]string{"message": "from the ref source", "replicas": "3"} {
		if data[key] != want {
			t.Errorf("Expected %s=%q from the ref source, got %q", key, want, data[key])
		}
	}
}

// Several value files out of the same ref source keep helm's own precedence: the
// last one listed wins.
func TestRefSourceValueFilesAreOrdered(t *testing.T) {
	root := refRepo(t)

	_, data := renderRefApp(t, root, refApp("$values/values/prod.yaml", "$values/values/extra.yaml"))

	if data["replicas"] != "9" {
		t.Errorf("Expected the last value file listed to win with replicas=9, got %q", data["replicas"])
	}
	// A key only the earlier file sets still has to come through.
	if data["message"] != "from the ref source" {
		t.Errorf("Expected message to survive from the earlier value file, got %q", data["message"])
	}
}

// A value file that is not prefixed with a $ref is still read from next to the
// chart, so declaring a ref source must not change how the ordinary ones resolve.
func TestRefSourceLeavesPlainValueFilesAlone(t *testing.T) {
	root := refRepo(t)

	if err := os.WriteFile(filepath.Join(root, "chart", "local.yaml"), []byte("message: from next to the chart\n"), 0o644); err != nil {
		t.Fatalf("Failed to write local.yaml: %v", err)
	}

	_, data := renderRefApp(t, root, refApp("$values/values/prod.yaml", "local.yaml"))

	if data["message"] != "from next to the chart" {
		t.Errorf("Expected the chart-local value file to be read as before, got %q", data["message"])
	}
	if data["replicas"] != "3" {
		t.Errorf("Expected the ref source's values to still apply, got replicas=%q", data["replicas"])
	}
}

// The ref source is there to be read from, not rendered. It has no path, and an
// empty path is the checkout root — so rendering it would pull in every manifest
// that happens to lie at the top of the repository.
func TestRefOnlySourceRendersNothing(t *testing.T) {
	root := refRepo(t)

	result, _ := renderRefApp(t, root, refApp("$values/values/prod.yaml"))

	for _, name := range renderedObjectNames(result) {
		if name == "stray" {
			t.Errorf("Expected the ref source to render nothing, but it rendered the repository root: %v", renderedObjectNames(result))
		}
	}

	if len(result.Objects) != 1 {
		t.Errorf("Expected only the chart's own manifest, got %v", renderedObjectNames(result))
	}

	// Only the chart source produces manifests, so only it counts as rendered.
	if result.SourcesProcessed != 1 {
		t.Errorf("Expected 1 source rendered, got %d", result.SourcesProcessed)
	}
}

// A $name nobody declared used to resolve to nothing at all: the unknown variable
// expands to an empty string and what is left is read from the checkout root, so
// the chart was rendered against the wrong file — or silently against its own
// defaults — instead of the render failing.
func TestUndeclaredRefIsRejected(t *testing.T) {
	root := refRepo(t)

	for _, tc := range []struct {
		name      string
		app       string
		wantError string
	}{
		{
			name:      "typo in the ref name",
			app:       refApp("$vaules/values/prod.yaml"),
			wantError: `"$vaules"`,
		},
		{
			name: "no source declares a ref at all",
			app: `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: ref-app
spec:
  project: default
  sources:
  - repoURL: https://github.com/myorg/myrepo
    path: chart
    helm:
      valueFiles:
      - $values/values/prod.yaml
  - repoURL: https://github.com/myorg/myrepo
    path: chart
  destination:
    server: https://kubernetes.default.svc
    namespace: ref-namespace
`,
			wantError: "no source has a 'ref' field defined",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := TemplateFromApplicationYAML(context.Background(), tc.app, TemplateOptions{RepoRoot: root})
			if err == nil {
				t.Fatal("Expected an undeclared $ref to be refused")
			}
			if !strings.Contains(err.Error(), tc.wantError) {
				t.Errorf("Expected the error to mention %s, got %v", tc.wantError, err)
			}
		})
	}
}

// Two sources claiming the same $name leave it ambiguous which checkout a value
// file comes from, so the Application is refused rather than rendered from
// whichever one map iteration happened to land on.
func TestDuplicateRefKeysAreRejected(t *testing.T) {
	root := refRepo(t)

	app := `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: ref-app
spec:
  project: default
  sources:
  - repoURL: https://github.com/myorg/myrepo
    ref: values
  - repoURL: https://github.com/myorg/other
    ref: values
  - repoURL: https://github.com/myorg/myrepo
    path: chart
    helm:
      valueFiles:
      - $values/values/prod.yaml
  destination:
    server: https://kubernetes.default.svc
    namespace: ref-namespace
`

	_, err := TemplateFromApplicationYAML(context.Background(), app, TemplateOptions{RepoRoot: root})
	if err == nil {
		t.Fatal("Expected two sources sharing a 'ref' key to be refused")
	}
	if !strings.Contains(err.Error(), "same 'ref' key") {
		t.Errorf("Expected the error to name the duplicate key, got %v", err)
	}
}

// A ref path is resolved inside the referenced checkout, so one that climbs out
// of it has to be refused the same way an ordinary value file is.
func TestRefPathCannotEscapeTheCheckout(t *testing.T) {
	root := refRepo(t)

	outside := filepath.Join(filepath.Dir(root), "outside.yaml")
	if err := os.WriteFile(outside, []byte("message: outside the checkout\n"), 0o644); err != nil {
		t.Fatalf("Failed to write %s: %v", outside, err)
	}
	t.Cleanup(func() { os.Remove(outside) })

	_, err := TemplateFromApplicationYAML(
		context.Background(),
		refApp("$values/../"+filepath.Base(outside)),
		TemplateOptions{RepoRoot: root},
	)
	if err == nil {
		t.Fatal("Expected a $ref path pointing outside the checkout to be refused")
	}
}

// $ref means nothing to a single-source Application: there is no sibling source to
// refer to, and the repo-server treats a leading $ there as a build environment
// variable instead. An unset one expands to nothing, which leaves an absolute path
// read from the checkout root — odd, but it is what Argo CD does, and a renderer
// that quietly disagreed with it would be worse than one that does not.
func TestRefInSingleSourceApplicationIsNotARef(t *testing.T) {
	root := refRepo(t)

	app := `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: ref-app
spec:
  project: default
  source:
    repoURL: https://github.com/myorg/myrepo
    path: chart
    helm:
      valueFiles:
      - $values/values/prod.yaml
  destination:
    server: https://kubernetes.default.svc
    namespace: ref-namespace
`

	result, err := TemplateFromApplicationYAML(context.Background(), app, TemplateOptions{RepoRoot: root})
	if err != nil {
		t.Fatalf("Failed to template application: %v", err)
	}

	if result.SourcesProcessed != 1 {
		t.Errorf("Expected the single source to be rendered, got %d", result.SourcesProcessed)
	}
}

// An Application whose every source is a ref source renders nothing at all. That
// is a mistake in the manifest, and saying so beats handing back an empty result.
func TestApplicationOfOnlyRefSourcesIsRejected(t *testing.T) {
	root := refRepo(t)

	app := `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: ref-app
spec:
  project: default
  sources:
  - repoURL: https://github.com/myorg/myrepo
    ref: values
  - repoURL: https://github.com/myorg/myrepo
    ref: other
  destination:
    server: https://kubernetes.default.svc
    namespace: ref-namespace
`

	_, err := TemplateFromApplicationYAML(context.Background(), app, TemplateOptions{RepoRoot: root})
	if err == nil {
		t.Fatal("Expected an Application with nothing to render to be refused")
	}
	if !strings.Contains(err.Error(), "no renderable sources") {
		t.Errorf("Expected the error to say there is nothing to render, got %v", err)
	}
}

// A source that names a ref *and* carries a path of its own is a normal source
// that other sources may also read files from, so it still renders.
func TestSourceWithBothRefAndPathStillRenders(t *testing.T) {
	root := refRepo(t)

	app := `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: ref-app
spec:
  project: default
  sources:
  - repoURL: https://github.com/myorg/myrepo
    path: shared
    ref: values
  - repoURL: https://github.com/myorg/myrepo
    path: chart
    helm:
      valueFiles:
      - $values/values/prod.yaml
  destination:
    server: https://kubernetes.default.svc
    namespace: ref-namespace
`

	result, err := TemplateFromApplicationYAML(context.Background(), app, TemplateOptions{RepoRoot: root})
	if err != nil {
		t.Fatalf("Failed to template application: %v", err)
	}

	if result.SourcesProcessed != 2 {
		t.Errorf("Expected both sources to be rendered, got %d", result.SourcesProcessed)
	}
}
