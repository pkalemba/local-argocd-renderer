package renderer

import (
	"strings"
	"testing"
)

const argocdNamespace = "argocd"

func clusterNames(t *testing.T, data string) []string {
	t.Helper()

	secrets, err := parseClusterSecrets([]byte(data), argocdNamespace)
	if err != nil {
		t.Fatalf("parseClusterSecrets failed: %v", err)
	}

	var names []string
	for _, secret := range secrets {
		if secret.Namespace != argocdNamespace {
			t.Errorf("Expected secret %q in %s, got %q", secret.Name, argocdNamespace, secret.Namespace)
		}
		names = append(names, secret.Name)
	}

	return names
}

// The README hands people `kubectl -n argocd get secret -l ... -o yaml`, and kubectl
// wraps anything but a single result in a `kind: List`. That was rejected outright
// with "expected kind 'Secret', got 'List'", so the documented export never worked.
func TestClusterSecretsFromAKubectlList(t *testing.T) {
	names := clusterNames(t, `
apiVersion: v1
kind: List
metadata:
  resourceVersion: ""
items:
- apiVersion: v1
  kind: Secret
  metadata:
    name: staging
    namespace: some-other-namespace
    labels:
      argocd.argoproj.io/secret-type: cluster
  data:
    name: c3RhZ2luZw==
    server: aHR0cHM6Ly9zdGFnaW5nLmV4YW1wbGUuY29t
- apiVersion: v1
  kind: Secret
  metadata:
    name: production
    labels:
      argocd.argoproj.io/secret-type: cluster
  stringData:
    name: production
    server: https://production.example.com
`)

	if len(names) != 2 || names[0] != "staging" || names[1] != "production" {
		t.Errorf("Expected both clusters from the list, got %v", names)
	}
}

// A single-item export is not wrapped, and multi-document files were already the
// hand-written form. Both still have to work.
func TestClusterSecretsFromPlainDocuments(t *testing.T) {
	names := clusterNames(t, `
apiVersion: v1
kind: Secret
metadata:
  name: one
  labels:
    argocd.argoproj.io/secret-type: cluster
stringData:
  name: one
  server: https://one.example.com
---
# A comment-only document, and an explicit null, are both skipped rather than
# reported as a nameless secret.
---
null
---
apiVersion: v1
kind: Secret
metadata:
  name: two
  labels:
    argocd.argoproj.io/secret-type: cluster
stringData:
  name: two
  server: https://two.example.com
`)

	if len(names) != 2 || names[0] != "one" || names[1] != "two" {
		t.Errorf("Expected both clusters, got %v", names)
	}
}

func TestClusterSecretsRejectOtherKinds(t *testing.T) {
	for _, tc := range []struct {
		name     string
		data     string
		contains string
	}{
		{
			name:     "a kind that is not a Secret",
			data:     "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: nope\n",
			contains: "expected kind 'Secret'",
		},
		{
			// Inside a list too, rather than being quietly dropped.
			name:     "a stray kind inside a list",
			data:     "apiVersion: v1\nkind: List\nitems:\n- apiVersion: v1\n  kind: ConfigMap\n  metadata:\n    name: nope\n",
			contains: "expected kind 'Secret'",
		},
		{
			name:     "a secret the generator would ignore",
			data:     "apiVersion: v1\nkind: Secret\nmetadata:\n  name: unlabelled\n",
			contains: "is missing the argocd.argoproj.io/secret-type=cluster label",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseClusterSecrets([]byte(tc.data), argocdNamespace)
			if err == nil {
				t.Fatal("Expected an error")
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Errorf("Expected the error to mention %q, got %v", tc.contains, err)
			}
		})
	}
}

// stringData is folded into data the way the API server does on write, because the
// fake client the generator reads through does not.
func TestClusterSecretsFoldStringData(t *testing.T) {
	secrets, err := parseClusterSecrets([]byte(`
apiVersion: v1
kind: Secret
metadata:
  name: staging
  labels:
    argocd.argoproj.io/secret-type: cluster
stringData:
  name: staging
  server: https://staging.example.com
`), argocdNamespace)
	if err != nil {
		t.Fatalf("parseClusterSecrets failed: %v", err)
	}

	if len(secrets) != 1 {
		t.Fatalf("Expected one secret, got %d", len(secrets))
	}
	if got := string(secrets[0].Data["server"]); got != "https://staging.example.com" {
		t.Errorf("Expected stringData folded into data, got %q", got)
	}
	if secrets[0].StringData != nil {
		t.Error("Expected stringData to be cleared once folded")
	}
}
