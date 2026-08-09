package renderer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// A render of a directory holding one manifest per name, into dest-ns.
func renderInto(t *testing.T, manifests map[string]string) map[string]string {
	t.Helper()

	root, err := os.MkdirTemp(".", "scope-*")
	if err != nil {
		t.Fatalf("Failed to create a fixture directory: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })

	input := filepath.Join(root, "input")
	if err := os.MkdirAll(input, 0o755); err != nil {
		t.Fatalf("Failed to create %s: %v", input, err)
	}
	for name, manifest := range manifests {
		if err := os.WriteFile(filepath.Join(input, name+".yaml"), []byte(manifest), 0o644); err != nil {
			t.Fatalf("Failed to write %s: %v", name, err)
		}
	}

	app := `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: scope-app
spec:
  project: default
  source:
    repoURL: https://github.com/argoproj/argo-cd
    path: ` + filepath.ToSlash(input) + `
  destination:
    server: https://kubernetes.default.svc
    namespace: dest-ns
`
	appPath := filepath.Join(root, "app.yaml")
	if err := os.WriteFile(appPath, []byte(app), 0o644); err != nil {
		t.Fatalf("Failed to write %s: %v", appPath, err)
	}

	result, err := Template(context.Background(), TemplateOptions{ApplicationFile: appPath, RepoRoot: "."})
	if err != nil {
		t.Fatalf("Template failed: %v", err)
	}

	namespaces := map[string]string{}
	for _, obj := range result.Objects {
		namespaces[obj.GetKind()] = obj.GetNamespace()
	}

	return namespaces
}

// Argo CD asks the API server whether a kind is namespaced and leaves cluster-scoped
// resources alone. The stub this replaced answered "namespaced" for everything, so
// a Namespace, a ClusterRole and a CRD all came out carrying the destination
// namespace — output that does not match what the cluster would run.
func TestClusterScopedResourcesKeepNoNamespace(t *testing.T) {
	namespaces := renderInto(t, map[string]string{
		"deployment":   "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: app\nspec:\n  selector:\n    matchLabels:\n      a: b\n  template:\n    metadata:\n      labels:\n        a: b\n    spec:\n      containers:\n      - name: c\n        image: busybox\n",
		"namespace":    "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: created-ns\n",
		"clusterrole":  "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata:\n  name: reader\nrules: []\n",
		"storageclass": "apiVersion: storage.k8s.io/v1\nkind: StorageClass\nmetadata:\n  name: fast\nprovisioner: kubernetes.io/no-provisioner\n",
	})

	for _, kind := range []string{"Namespace", "ClusterRole", "StorageClass"} {
		if got := namespaces[kind]; got != "" {
			t.Errorf("Expected %s to carry no namespace, got %q", kind, got)
		}
	}

	// Namespaced resources must still be moved into the destination namespace.
	if got := namespaces["Deployment"]; got != "dest-ns" {
		t.Errorf("Expected the Deployment in dest-ns, got %q", got)
	}
}

// A chart that ships its own CRDs tells us the scope of its custom resources, so
// those do not have to be guessed at.
func TestScopeIsReadFromRenderedDefinitions(t *testing.T) {
	namespaces := renderInto(t, map[string]string{
		"crd": `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: clusterwidgets.example.com
spec:
  group: example.com
  scope: Cluster
  names:
    kind: ClusterWidget
    plural: clusterwidgets
  versions: []
`,
		"cr": "apiVersion: example.com/v1\nkind: ClusterWidget\nmetadata:\n  name: w\n",
	})

	if got := namespaces["ClusterWidget"]; got != "" {
		t.Errorf("Expected the custom resource to follow its CRD's Cluster scope, got namespace %q", got)
	}
	if got := namespaces["CustomResourceDefinition"]; got != "" {
		t.Errorf("Expected the CRD itself to carry no namespace, got %q", got)
	}
}

func TestUnknownKindsAreTreatedAsNamespaced(t *testing.T) {
	provider := newResourceScopeProvider(nil)

	// gitops-engine's IsNamespacedOrUnknown treats an unknown kind as namespaced;
	// answering the same way keeps a custom resource in its destination namespace
	// rather than silently dropping it out of one.
	namespaced, err := provider.IsNamespaced(schema.GroupKind{Group: "example.com", Kind: "Widget"})
	if err != nil {
		t.Fatalf("IsNamespaced failed: %v", err)
	}
	if !namespaced {
		t.Error("Expected an unknown kind to be treated as namespaced")
	}

	clusterScoped, err := provider.IsNamespaced(schema.GroupKind{Kind: "Namespace"})
	if err != nil {
		t.Fatalf("IsNamespaced failed: %v", err)
	}
	if clusterScoped {
		t.Error("Expected Namespace to be cluster-scoped")
	}
}
