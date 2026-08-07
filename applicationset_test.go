package renderer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

const listAppSet = `
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: test-appset
spec:
  goTemplate: true
  generators:
    - list:
        elements:
          - name: one
          - name: two
  template:
    metadata:
      name: test-{{ .name }}
    spec:
      project: default
      source:
        repoURL: https://github.com/argoproj/argo-cd
        path: examples/appset-list/input/{{ .name }}
      destination:
        server: https://kubernetes.default.svc
        namespace: default
`

func mustParseApplicationSet(t *testing.T, content string) *v1alpha1.ApplicationSet {
	t.Helper()

	var appSet v1alpha1.ApplicationSet
	if err := yaml.Unmarshal([]byte(content), &appSet); err != nil {
		t.Fatalf("Failed to parse ApplicationSet: %v", err)
	}

	return &appSet
}

func TestGenerateApplications(t *testing.T) {
	apps, _, err := GenerateApplications(mustParseApplicationSet(t, listAppSet), TemplateOptions{RepoRoot: "."})
	if err != nil {
		t.Fatalf("GenerateApplications failed: %v", err)
	}

	if len(apps) != 2 {
		t.Fatalf("Expected 2 generated applications, got %d", len(apps))
	}

	for i, expected := range []string{"test-one", "test-two"} {
		if apps[i].Name != expected {
			t.Errorf("Expected application %d to be named %q, got %q", i, expected, apps[i].Name)
		}
	}

	if got := apps[0].Spec.GetSource().Path; got != "examples/appset-list/input/one" {
		t.Errorf("Expected the template parameter to be substituted in the source path, got %q", got)
	}
}

func TestGenerateApplicationsFromGitDirectories(t *testing.T) {
	appSet := mustParseApplicationSet(t, `
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: test-git-appset
spec:
  goTemplate: true
  generators:
    - git:
        repoURL: https://github.com/argoproj/argo-cd
        revision: HEAD
        directories:
          - path: examples/appset-git/input/*
  template:
    metadata:
      name: test-{{ .path.basename }}
    spec:
      project: default
      source:
        repoURL: https://github.com/argoproj/argo-cd
        path: '{{ .path.path }}'
      destination:
        server: https://kubernetes.default.svc
        namespace: default
`)

	apps, _, err := GenerateApplications(appSet, TemplateOptions{RepoRoot: "."})
	if err != nil {
		t.Fatalf("GenerateApplications failed: %v", err)
	}

	if len(apps) != 2 {
		t.Fatalf("Expected 2 generated applications, got %d", len(apps))
	}

	for i, expected := range []string{"test-alpha", "test-beta"} {
		if apps[i].Name != expected {
			t.Errorf("Expected application %d to be named %q, got %q", i, expected, apps[i].Name)
		}
	}
}

func TestGenerateApplicationsFromGitFiles(t *testing.T) {
	appSet := mustParseApplicationSet(t, `
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: test-git-files-appset
spec:
  goTemplate: true
  generators:
    - git:
        repoURL: https://github.com/argoproj/argo-cd
        revision: HEAD
        files:
          - path: examples/appset-git/input/*/configmap.yaml
  template:
    metadata:
      name: test-{{ .metadata.name }}
    spec:
      project: default
      source:
        repoURL: https://github.com/argoproj/argo-cd
        path: examples/appset-git/input/{{ .metadata.name }}
      destination:
        server: https://kubernetes.default.svc
        namespace: default
`)

	apps, _, err := GenerateApplications(appSet, TemplateOptions{RepoRoot: "."})
	if err != nil {
		t.Fatalf("GenerateApplications failed: %v", err)
	}

	if len(apps) != 2 {
		t.Fatalf("Expected 2 generated applications, got %d", len(apps))
	}
}

const clusterAppSet = `
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: test-cluster-appset
spec:
  goTemplate: true
  generators:
    - clusters: {}
  template:
    metadata:
      name: test-{{ .nameNormalized }}
    spec:
      project: default
      source:
        repoURL: https://github.com/argoproj/argo-cd
        path: examples/directory/input
      destination:
        server: '{{ .server }}'
        namespace: default
`

const clusterSecrets = `
apiVersion: v1
kind: Secret
metadata:
  name: staging-cluster
  labels:
    argocd.argoproj.io/secret-type: cluster
stringData:
  name: staging
  server: https://staging.example.com
---
apiVersion: v1
kind: Secret
metadata:
  name: production-cluster
  labels:
    argocd.argoproj.io/secret-type: cluster
stringData:
  name: production
  server: https://production.example.com
`

func writeFile(t *testing.T, name, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("Failed to write %s: %v", path, err)
	}

	return path
}

func TestGenerateApplicationsFromClusters(t *testing.T) {
	appSet := mustParseApplicationSet(t, clusterAppSet)

	apps, warnings, err := GenerateApplications(appSet, TemplateOptions{
		RepoRoot:     ".",
		ClustersFile: writeFile(t, "clusters.yaml", clusterSecrets),
	})
	if err != nil {
		t.Fatalf("GenerateApplications failed: %v", err)
	}

	if len(warnings) != 0 {
		t.Errorf("Expected no warnings when cluster secrets are supplied, got %v", warnings)
	}

	// The in-cluster entry is always reported by the generator, on top of the two
	// clusters that were supplied.
	names := map[string]bool{}
	for _, app := range apps {
		names[app.Name] = true
	}

	for _, expected := range []string{"test-in-cluster", "test-staging", "test-production"} {
		if !names[expected] {
			t.Errorf("Expected an application named %q, got %v", expected, names)
		}
	}
}

func TestClusterGeneratorSelectsAndTemplatesOnLabels(t *testing.T) {
	appSet := mustParseApplicationSet(t, `
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: test-cluster-labels-appset
spec:
  goTemplate: true
  generators:
    - clusters:
        selector:
          matchLabels:
            environment: staging
  template:
    metadata:
      name: test-{{ .nameNormalized }}-{{ .metadata.labels.environment }}
    spec:
      project: default
      source:
        repoURL: https://github.com/argoproj/argo-cd
        path: examples/directory/input
      destination:
        server: '{{ .server }}'
        namespace: default
`)

	path := writeFile(t, "clusters.yaml", `
apiVersion: v1
kind: Secret
metadata:
  name: staging-cluster
  labels:
    argocd.argoproj.io/secret-type: cluster
    environment: staging
stringData:
  name: staging
  server: https://staging.example.com
---
apiVersion: v1
kind: Secret
metadata:
  name: production-cluster
  labels:
    argocd.argoproj.io/secret-type: cluster
    environment: production
stringData:
  name: production
  server: https://production.example.com
`)

	apps, _, err := GenerateApplications(appSet, TemplateOptions{RepoRoot: ".", ClustersFile: path})
	if err != nil {
		t.Fatalf("GenerateApplications failed: %v", err)
	}

	// A non-empty selector also excludes the local in-cluster entry.
	if len(apps) != 1 {
		t.Fatalf("Expected the selector to match 1 cluster, got %d applications", len(apps))
	}

	if apps[0].Name != "test-staging-staging" {
		t.Errorf("Expected the cluster labels to be available as parameters, got %q", apps[0].Name)
	}
}

func TestClusterGeneratorWithoutClustersFileWarns(t *testing.T) {
	apps, warnings, err := GenerateApplications(mustParseApplicationSet(t, clusterAppSet), TemplateOptions{RepoRoot: "."})
	if err != nil {
		t.Fatalf("GenerateApplications failed: %v", err)
	}

	if len(apps) != 1 || apps[0].Name != "test-in-cluster" {
		t.Errorf("Expected only the in-cluster application, got %d applications", len(apps))
	}

	if len(warnings) != 1 || !strings.Contains(warnings[0], "in-cluster") {
		t.Errorf("Expected a warning about the missing cluster secrets, got %v", warnings)
	}
}

func TestClusterSecretWithoutLabelIsRejected(t *testing.T) {
	path := writeFile(t, "clusters.yaml", `
apiVersion: v1
kind: Secret
metadata:
  name: staging-cluster
stringData:
  name: staging
  server: https://staging.example.com
`)

	_, _, err := GenerateApplications(mustParseApplicationSet(t, clusterAppSet), TemplateOptions{
		RepoRoot:     ".",
		ClustersFile: path,
	})
	if err == nil {
		t.Fatal("Expected an error for a secret without the cluster secret-type label")
	}
}

func TestUnsupportedGeneratorReportsError(t *testing.T) {
	appSet := mustParseApplicationSet(t, `
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: test-scm-appset
spec:
  generators:
    - scmProvider:
        github:
          organization: myorg
  template:
    metadata:
      name: test-{{repository}}
    spec:
      project: default
      source:
        repoURL: https://github.com/argoproj/argo-cd
        path: examples/directory/input
      destination:
        server: https://kubernetes.default.svc
        namespace: default
`)

	_, _, err := GenerateApplications(appSet, TemplateOptions{RepoRoot: "."})
	if err == nil {
		t.Fatal("Expected an error for a generator that queries a remote service")
	}
	if !strings.Contains(err.Error(), "scmProvider") {
		t.Errorf("Expected the error to name the scmProvider generator, got: %v", err)
	}
}

func TestTemplateFromApplicationSetYAML(t *testing.T) {
	ctx := context.Background()

	result, err := TemplateFromApplicationSetYAML(ctx, listAppSet, ".")
	if err != nil {
		t.Fatalf("TemplateFromApplicationSetYAML failed: %v", err)
	}

	if result.ApplicationsProcessed != 2 {
		t.Errorf("Expected 2 applications processed, got %d", result.ApplicationsProcessed)
	}

	if len(result.Objects) != 2 {
		t.Fatalf("Expected 2 objects, got %d", len(result.Objects))
	}

	if len(result.Applications) != 2 {
		t.Fatalf("Expected the objects to be grouped into 2 applications, got %d", len(result.Applications))
	}

	for i, expected := range []string{"test-one", "test-two"} {
		if result.Applications[i].Name != expected {
			t.Errorf("Expected group %d to be named %q, got %q", i, expected, result.Applications[i].Name)
		}
		if len(result.Applications[i].Objects) != 1 {
			t.Errorf("Expected group %d to hold 1 object, got %d", i, len(result.Applications[i].Objects))
		}
	}
}

func TestTemplateRejectsUnknownKind(t *testing.T) {
	ctx := context.Background()

	_, err := TemplateFromYAML(ctx, "apiVersion: v1\nkind: ConfigMap\n", TemplateOptions{RepoRoot: "."})
	if err == nil {
		t.Fatal("Expected an error for an unsupported kind")
	}
}
