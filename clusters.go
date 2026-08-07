package renderer

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/argoproj/argo-cd/v3/common"
)

// loadClusterSecrets reads Argo CD cluster secrets from a file or from every
// *.yaml/*.yml file in a directory. These are the very same secrets the
// ApplicationSet controller reads from the Argo CD namespace, so an export of
//
//	kubectl -n argocd get secret -l argocd.argoproj.io/secret-type=cluster -o yaml
//
// can be fed in as-is. Every secret is moved into namespace so that the generator,
// which looks in the Argo CD namespace, finds it.
func loadClusterSecrets(path, namespace string) ([]corev1.Secret, error) {
	files, err := clusterSecretFiles(path)
	if err != nil {
		return nil, err
	}

	var secrets []corev1.Secret
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("failed to read cluster secrets from %q: %w", file, err)
		}

		fileSecrets, err := parseClusterSecrets(data, namespace)
		if err != nil {
			return nil, fmt.Errorf("failed to parse cluster secrets from %q: %w", file, err)
		}

		secrets = append(secrets, fileSecrets...)
	}

	return secrets, nil
}

func clusterSecretFiles(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read cluster secrets from %q: %w", path, err)
	}

	if !info.IsDir() {
		return []string{path}, nil
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("failed to list cluster secrets in %q: %w", path, err)
	}

	var files []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		switch filepath.Ext(entry.Name()) {
		case ".yaml", ".yml":
			files = append(files, filepath.Join(path, entry.Name()))
		}
	}

	return files, nil
}

// mergeStringData folds stringData into data the way the API server does on write,
// so that hand-written cluster secrets are read the same as exported ones.
func mergeStringData(secret *corev1.Secret) *corev1.Secret {
	if len(secret.StringData) == 0 {
		return secret
	}

	if secret.Data == nil {
		secret.Data = make(map[string][]byte, len(secret.StringData))
	}
	for key, value := range secret.StringData {
		secret.Data[key] = []byte(value)
	}
	secret.StringData = nil

	return secret
}

func parseClusterSecrets(data []byte, namespace string) ([]corev1.Secret, error) {
	var secrets []corev1.Secret

	reader := k8syaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	for {
		doc, err := reader.Read()
		if err == io.EOF {
			return secrets, nil
		}
		if err != nil {
			return nil, err
		}
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}

		var secret corev1.Secret
		if err := yaml.Unmarshal(doc, &secret); err != nil {
			return nil, err
		}

		if secret.Kind != "" && secret.Kind != "Secret" {
			return nil, fmt.Errorf("expected kind 'Secret', got '%s'", secret.Kind)
		}
		if secret.Labels[common.LabelKeySecretType] != common.LabelValueSecretTypeCluster {
			return nil, fmt.Errorf("secret %q is missing the %s=%s label and would be ignored by the cluster generator",
				secret.Name, common.LabelKeySecretType, common.LabelValueSecretTypeCluster)
		}

		// The generator reads the clusters from the Argo CD namespace.
		secret.Namespace = namespace
		secrets = append(secrets, *mergeStringData(&secret))
	}
}
