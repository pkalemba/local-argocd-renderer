package renderer

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// clusterScopedKinds are the built-in Kubernetes kinds that do not live in a
// namespace. Argo CD asks the API server; without a cluster this is the closest
// stand-in that does not require one.
//
// Anything absent is reported as namespaced. That is the safe direction: the vast
// majority of kinds are namespaced, gitops-engine already treats "unknown" that way
// (IsNamespacedOrUnknown returns namespaced || err != nil), and a namespace on a
// resource that does not take one is a visible mistake, whereas a missing one is
// silently wrong.
var clusterScopedKinds = map[schema.GroupKind]bool{
	{Group: "", Kind: "Namespace"}:        true,
	{Group: "", Kind: "Node"}:             true,
	{Group: "", Kind: "PersistentVolume"}: true,
	{Group: "", Kind: "ComponentStatus"}:  true,

	{Group: "rbac.authorization.k8s.io", Kind: "ClusterRole"}:        true,
	{Group: "rbac.authorization.k8s.io", Kind: "ClusterRoleBinding"}: true,

	{Group: "apiextensions.k8s.io", Kind: "CustomResourceDefinition"}: true,
	{Group: "apiregistration.k8s.io", Kind: "APIService"}:             true,

	{Group: "admissionregistration.k8s.io", Kind: "MutatingWebhookConfiguration"}:     true,
	{Group: "admissionregistration.k8s.io", Kind: "ValidatingWebhookConfiguration"}:   true,
	{Group: "admissionregistration.k8s.io", Kind: "ValidatingAdmissionPolicy"}:        true,
	{Group: "admissionregistration.k8s.io", Kind: "ValidatingAdmissionPolicyBinding"}: true,
	{Group: "admissionregistration.k8s.io", Kind: "MutatingAdmissionPolicy"}:          true,
	{Group: "admissionregistration.k8s.io", Kind: "MutatingAdmissionPolicyBinding"}:   true,

	{Group: "storage.k8s.io", Kind: "StorageClass"}:          true,
	{Group: "storage.k8s.io", Kind: "VolumeAttachment"}:      true,
	{Group: "storage.k8s.io", Kind: "CSIDriver"}:             true,
	{Group: "storage.k8s.io", Kind: "CSINode"}:               true,
	{Group: "storage.k8s.io", Kind: "VolumeAttributesClass"}: true,

	{Group: "scheduling.k8s.io", Kind: "PriorityClass"}: true,
	{Group: "node.k8s.io", Kind: "RuntimeClass"}:        true,
	{Group: "networking.k8s.io", Kind: "IngressClass"}:  true,
	{Group: "policy", Kind: "PodSecurityPolicy"}:        true,

	{Group: "certificates.k8s.io", Kind: "CertificateSigningRequest"}: true,
	{Group: "certificates.k8s.io", Kind: "ClusterTrustBundle"}:        true,

	{Group: "flowcontrol.apiserver.k8s.io", Kind: "FlowSchema"}:                 true,
	{Group: "flowcontrol.apiserver.k8s.io", Kind: "PriorityLevelConfiguration"}: true,

	{Group: "resource.k8s.io", Kind: "DeviceClass"}: true,
}

// resourceScopeProvider answers the question Argo CD normally puts to the API
// server: does this kind live in a namespace?
type resourceScopeProvider struct {
	// clusterScoped holds the kinds of any CustomResourceDefinitions rendered
	// alongside the resources, which is how a chart that ships its own CRDs gets
	// its custom resources scoped correctly.
	fromDefinitions map[schema.GroupKind]bool
}

// newResourceScopeProvider reads the scope of every CustomResourceDefinition among
// the objects, so that custom resources defined by the same render are not guessed
// at.
func newResourceScopeProvider(objects []*unstructured.Unstructured) *resourceScopeProvider {
	provider := &resourceScopeProvider{fromDefinitions: map[schema.GroupKind]bool{}}

	for _, obj := range objects {
		if obj.GetKind() != "CustomResourceDefinition" {
			continue
		}

		group, _, err := unstructured.NestedString(obj.Object, "spec", "group")
		if err != nil {
			continue
		}
		kind, _, err := unstructured.NestedString(obj.Object, "spec", "names", "kind")
		if err != nil || kind == "" {
			continue
		}
		scope, _, err := unstructured.NestedString(obj.Object, "spec", "scope")
		if err != nil {
			continue
		}

		provider.fromDefinitions[schema.GroupKind{Group: group, Kind: kind}] = scope == "Cluster"
	}

	return provider
}

func (p *resourceScopeProvider) IsNamespaced(gk schema.GroupKind) (bool, error) {
	if clusterScoped, defined := p.fromDefinitions[gk]; defined {
		return !clusterScoped, nil
	}

	return !clusterScopedKinds[gk], nil
}
