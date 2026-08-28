package renderer

import (
	"fmt"
	"os"

	"github.com/argoproj/argo-cd/v3/reposerver/apiclient"
	k8sversion "k8s.io/apimachinery/pkg/util/version"
	"sigs.k8s.io/yaml"
)

// HelmCapabilities describes the destination cluster the way `helm template`
// exposes it to a chart as `.Capabilities`, so that the guards charts write
// around it — `.Capabilities.APIVersions.Has "monitoring.coreos.com/v1"`, a
// comparison against `.Capabilities.KubeVersion.Version` — decide the same way
// here as they would against the real cluster.
//
// Without it Helm falls back to its built-in defaults: the Kubernetes version
// the helm binary was built against, and the API versions of a bare cluster. A
// chart is then rendered as if none of the CRDs it looks for were installed,
// which is exactly the part of a diff that matters.
type HelmCapabilities struct {
	// KubeVersion is the cluster's Kubernetes version, as `helm template
	// --kube-version` takes it, e.g. "v1.31.4". It fills in
	// .Capabilities.KubeVersion, from which Helm derives Major and Minor.
	KubeVersion string `json:"kubeVersion,omitempty"`
	// APIVersions lists the group/versions — and, where a chart checks for one,
	// the group/version/Kind — installed on the cluster, e.g.
	// "monitoring.coreos.com/v1" or "monitoring.coreos.com/v1/ServiceMonitor".
	//
	// Helm appends these to the set it always knows about, so only what a bare
	// cluster does not have has to be listed. The list an actual cluster
	// answers with is what `kubectl api-resources` reports.
	APIVersions []string `json:"apiVersions,omitempty"`
}

// LoadHelmCapabilities reads capabilities from a YAML file. An empty path means
// no file was asked for, which is not an error: it returns a nil
// *HelmCapabilities, and rendering falls back to Helm's own defaults.
//
// The file is parsed strictly, because a misspelled key in it would otherwise
// be indistinguishable from a cluster that really is missing the CRD — the
// charts would render, just as if the file had never been passed.
func LoadHelmCapabilities(path string) (*HelmCapabilities, error) {
	if path == "" {
		return nil, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read HelmCapabilitiesFile: %w", err)
	}

	var caps HelmCapabilities
	if err := yaml.UnmarshalStrict(data, &caps); err != nil {
		return nil, fmt.Errorf("failed to parse HelmCapabilitiesFile %q: %w", path, err)
	}

	if err := caps.validate(path); err != nil {
		return nil, err
	}

	return &caps, nil
}

// validate rejects a file that would only fail much later, once a chart is being
// rendered, with an error naming neither the file nor the field it came from.
func (caps *HelmCapabilities) validate(path string) error {
	if caps.KubeVersion != "" {
		if _, err := k8sversion.ParseGeneric(caps.KubeVersion); err != nil {
			return fmt.Errorf("HelmCapabilitiesFile %q: kubeVersion %q is not a Kubernetes version: %w", path, caps.KubeVersion, err)
		}
	}

	for i, apiVersion := range caps.APIVersions {
		if apiVersion == "" {
			return fmt.Errorf("HelmCapabilitiesFile %q: apiVersions[%d] is empty", path, i)
		}
	}

	return nil
}

// applyTo puts the capabilities on a manifest request, where they take the place
// of the cluster the repo-server would have read them from.
//
// They are a default rather than an override, which is how Argo CD itself treats
// the destination cluster's: a source that pins spec.source.helm.kubeVersion or
// spec.source.helm.apiVersions — spec.source.kustomize for a Kustomize source —
// keeps deciding for itself. Both fields are consulted for Helm and for
// Kustomize sources, so a chart rendered through `kustomize build --enable-helm`
// sees them too.
func (caps *HelmCapabilities) applyTo(req *apiclient.ManifestRequest) {
	if caps == nil {
		return
	}

	req.KubeVersion = caps.KubeVersion
	req.ApiVersions = caps.APIVersions
}
