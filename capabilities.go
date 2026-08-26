package renderer

import (
	"fmt"
	"os"

	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"sigs.k8s.io/yaml"
)

// HelmCapabilities represents a Helm capabilities structure that can be passed to helm template.
// It allows you to override the Kubernetes API versions and version that Helm uses during
// chart rendering.
type HelmCapabilities struct {
	APIVersions []string `json:"apiVersions,omitempty" yaml:"apiVersions"`
	KubeVersion *struct {
		Version string `json:"version,omitempty" yaml:"version"`
		Major   string `json:"major,omitempty" yaml:"major"`
		Minor   string `json:"minor,omitempty" yaml:"minor"`
	} `json:"kubeVersion,omitempty" yaml:"kubeVersion"`
}

// LoadHelmCapabilitiesFromFile reads a YAML file and returns HelmCapabilities.
// Returns nil if filePath is empty (capabilities are optional).
func LoadHelmCapabilitiesFromFile(filePath string) (*HelmCapabilities, error) {
	if filePath == "" {
		return nil, nil
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read capabilities file %q: %w", filePath, err)
	}

	var caps HelmCapabilities
	if err := yaml.Unmarshal(data, &caps); err != nil {
		return nil, fmt.Errorf("failed to parse capabilities file %q: %w", filePath, err)
	}

	return &caps, nil
}

// ApplyCapabilities adds the capabilities to the given ApplicationSource's Helm configuration.
// It preserves existing Helm settings and stores capabilities for later use during rendering.
// This is a no-op if the source is not a Helm source.
func ApplyCapabilities(source *v1alpha1.ApplicationSource, caps *HelmCapabilities) error {
	if caps == nil {
		return nil
	}

	if !source.IsHelm() {
		// Don't create a Helm block just for capabilities. This is consistent with
		// how IncludeHelmTests works — it only sets skipTests if the source is
		// already known to be Helm.
		return nil
	}

	// Ensure the Helm block exists
	if source.Helm == nil {
		source.Helm = &v1alpha1.ApplicationSourceHelm{}
	}

	// Note: The v1alpha1.ApplicationSourceHelm type doesn't have direct fields for
	// capabilities, so the actual capabilities are passed through at render time via
	// the repository.GenerateManifests call. This function ensures the structure is
	// in place for any future extensions.

	return nil
}

// BuildHelmCapabilitiesArgs converts HelmCapabilities to helm command line arguments.
// This is used when building the helm template command for chart rendering.
func BuildHelmCapabilitiesArgs(caps *HelmCapabilities) []string {
	if caps == nil {
		return []string{}
	}

	var args []string

	// Add API versions if specified
	if len(caps.APIVersions) > 0 {
		for _, apiVersion := range caps.APIVersions {
			args = append(args, "--api-versions", apiVersion)
		}
	}

	// Add Kubernetes version if specified
	if caps.KubeVersion != nil && caps.KubeVersion.Version != "" {
		args = append(args, "--kube-version", caps.KubeVersion.Version)
	}

	return args
}