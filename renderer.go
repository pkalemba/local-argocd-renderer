package renderer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"sigs.k8s.io/yaml"

	"github.com/argoproj/argo-cd/v3/controller"
	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v3/reposerver/apiclient"
	"github.com/argoproj/argo-cd/v3/reposerver/repository"
	"github.com/argoproj/argo-cd/v3/util/argo"
	"github.com/argoproj/argo-cd/v3/util/git"
)

const (
	appLabelKey    = "app.kubernetes.io/instance"
	installationID = "local-cli"
)

// TemplateOptions contains options for the templating process
type TemplateOptions struct {
	// ApplicationFile points at a manifest holding either an Application or an
	// ApplicationSet. Use "-" to read from stdin.
	ApplicationFile string
	RepoRoot        string
	MaxManifestSize string
	// ClustersFile points at a file or directory holding Argo CD cluster secrets.
	// Without it the ApplicationSet cluster generator only sees the in-cluster entry.
	ClustersFile string
}

// TemplateResult contains the results of the templating process
type TemplateResult struct {
	Objects  []*unstructured.Unstructured
	Warnings []string
	// SourcesProcessed is the total number of Application sources that were rendered.
	SourcesProcessed int
	// ApplicationsProcessed is the number of Applications that were rendered: one for
	// a plain Application, one per generated Application for an ApplicationSet.
	ApplicationsProcessed int
	// Applications holds the same objects as Objects, but grouped by the Application
	// they were rendered from.
	Applications []ApplicationResult
}

// ApplicationResult contains the manifests rendered from a single Application.
type ApplicationResult struct {
	Name             string
	Namespace        string
	Objects          []*unstructured.Unstructured
	Warnings         []string
	SourcesProcessed int
}

// Template processes an ArgoCD Application or ApplicationSet and returns templated
// manifests. The kind is taken from the manifest itself.
func Template(ctx context.Context, opts TemplateOptions) (*TemplateResult, error) {
	data, err := readManifest(opts.ApplicationFile)
	if err != nil {
		return nil, err
	}

	return TemplateFromYAML(ctx, string(data), opts)
}

// TemplateFromYAML processes an ArgoCD Application or ApplicationSet from YAML content.
func TemplateFromYAML(ctx context.Context, yamlContent string, opts TemplateOptions) (*TemplateResult, error) {
	var typeMeta metav1.TypeMeta
	if err := yaml.Unmarshal([]byte(yamlContent), &typeMeta); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}

	switch typeMeta.Kind {
	case "Application":
		app, err := parseApplication([]byte(yamlContent))
		if err != nil {
			return nil, err
		}
		return renderApplication(ctx, app, opts)
	case "ApplicationSet":
		appSet, err := parseApplicationSet([]byte(yamlContent))
		if err != nil {
			return nil, err
		}
		return renderApplicationSet(ctx, appSet, opts)
	default:
		return nil, fmt.Errorf("expected kind 'Application' or 'ApplicationSet', got '%s'", typeMeta.Kind)
	}
}

// TemplateFromApplication processes an ArgoCD Application and returns templated manifests
func TemplateFromApplication(ctx context.Context, opts TemplateOptions) (*TemplateResult, error) {
	data, err := readManifest(opts.ApplicationFile)
	if err != nil {
		return nil, err
	}

	app, err := parseApplication(data)
	if err != nil {
		return nil, fmt.Errorf("error parsing Application CRD: %w", err)
	}

	return renderApplication(ctx, app, opts)
}

// TemplateFromApplicationSet processes an ArgoCD ApplicationSet and returns the
// templated manifests of every Application it generates.
func TemplateFromApplicationSet(ctx context.Context, opts TemplateOptions) (*TemplateResult, error) {
	data, err := readManifest(opts.ApplicationFile)
	if err != nil {
		return nil, err
	}

	appSet, err := parseApplicationSet(data)
	if err != nil {
		return nil, fmt.Errorf("error parsing ApplicationSet CRD: %w", err)
	}

	return renderApplicationSet(ctx, appSet, opts)
}

// TemplateFromApplicationYAML processes an ArgoCD Application from YAML content
func TemplateFromApplicationYAML(ctx context.Context, yamlContent string, repoRoot string) (*TemplateResult, error) {
	app, err := parseApplication([]byte(yamlContent))
	if err != nil {
		return nil, fmt.Errorf("error parsing Application CRD: %w", err)
	}

	return renderApplication(ctx, app, TemplateOptions{RepoRoot: repoRoot})
}

// TemplateFromApplicationSetYAML processes an ArgoCD ApplicationSet from YAML content
func TemplateFromApplicationSetYAML(ctx context.Context, yamlContent string, repoRoot string) (*TemplateResult, error) {
	appSet, err := parseApplicationSet([]byte(yamlContent))
	if err != nil {
		return nil, fmt.Errorf("error parsing ApplicationSet CRD: %w", err)
	}

	return renderApplicationSet(ctx, appSet, TemplateOptions{RepoRoot: repoRoot})
}

// renderApplicationSet expands an ApplicationSet into Applications and renders each
// of them, concatenating the results.
func renderApplicationSet(ctx context.Context, appSet *v1alpha1.ApplicationSet, opts TemplateOptions) (*TemplateResult, error) {
	apps, warnings, err := GenerateApplications(appSet, opts)
	if err != nil {
		return nil, err
	}

	result := &TemplateResult{Warnings: warnings}
	for i := range apps {
		appResult, err := renderApplication(ctx, &apps[i], opts)
		if err != nil {
			return nil, fmt.Errorf("error rendering generated application %q: %w", apps[i].Name, err)
		}

		result.Objects = append(result.Objects, appResult.Objects...)
		result.Warnings = append(result.Warnings, appResult.Warnings...)
		result.SourcesProcessed += appResult.SourcesProcessed
		result.ApplicationsProcessed += appResult.ApplicationsProcessed
		result.Applications = append(result.Applications, appResult.Applications...)
	}

	return result, nil
}

// renderApplication renders every source of a single Application.
func renderApplication(ctx context.Context, app *v1alpha1.Application, opts TemplateOptions) (*TemplateResult, error) {
	requests, err := buildRequestsFromApplication(app)
	if err != nil {
		return nil, err
	}

	var allManifests []string
	var warnings []string

	// Process each source
	for sourceIndex, q := range requests {
		appPath := q.ApplicationSource.Path
		repoRoot := repoRootOrDefault(opts.RepoRoot)

		appSourceType, err := repository.GetAppSourceType(ctx, q.ApplicationSource, appPath, repoRoot, q.AppName, q.EnabledSourceTypes, []string{}, []string{})
		if err != nil {
			return nil, fmt.Errorf("error getting app source type: %w", err)
		}

		// For Kustomize sources, create a temporary overlay to avoid modifying the original
		if appSourceType == v1alpha1.ApplicationSourceTypeKustomize {
			tempDir, err := os.MkdirTemp(".", "kustomize-overlay-*")
			if err != nil {
				return nil, fmt.Errorf("error creating temp directory for Kustomize overlay: %w", err)
			}
			defer os.RemoveAll(tempDir)

			relPath, err := filepath.Rel(tempDir, appPath)
			if err != nil {
				os.RemoveAll(tempDir)
				return nil, fmt.Errorf("error calculating relative path: %w", err)
			}

			// Create a kustomization.yaml that references the original path
			kustomizationContent := fmt.Sprintf(`apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- %s
`, relPath)

			kustomizationPath := filepath.Join(tempDir, "kustomization.yaml")
			if err := os.WriteFile(kustomizationPath, []byte(kustomizationContent), 0644); err != nil {
				os.RemoveAll(tempDir)
				return nil, fmt.Errorf("error writing kustomization.yaml: %w", err)
			}

			appPath = tempDir
		}

		maxSize := resource.MustParse("10Mi")
		if opts.MaxManifestSize != "" {
			maxSize = resource.MustParse(opts.MaxManifestSize)
		}

		// Call the core GenerateManifests function directly
		response, err := repository.GenerateManifests(
			ctx,
			appPath,               // app path within repo
			repoRoot,              // repo root (current directory)
			"",                    // revision (empty for local files)
			q,                     // manifest request
			true,                  // isLocal=true - crucial for local operation!
			&git.NoopCredsStore{}, // no git credentials needed
			maxSize,               // max combined manifest size
			nil,                   // no temp paths needed for local operation
		)

		if err != nil {
			return nil, fmt.Errorf("error generating manifests for source %d: %w", sourceIndex+1, err)
		}

		// Collect manifests from this source
		allManifests = append(allManifests, response.Manifests...)
	}

	// Parse manifests into unstructured objects for deduplication
	var targetObjects []*unstructured.Unstructured
	for _, manifest := range allManifests {
		var obj unstructured.Unstructured
		if err := json.Unmarshal([]byte(manifest), &obj); err != nil {
			warnings = append(warnings, fmt.Sprintf("Failed to parse manifest as JSON: %v", err))
			continue
		}
		targetObjects = append(targetObjects, &obj)
	}

	// Normalize and deduplicate target objects using the library function. The
	// callback re-applies the tracking label whenever a namespace is filled in or
	// cleared, exactly as the application controller does.
	infoProvider := &resourceInfoProviderStub{}
	resourceTracking := argo.NewResourceTracking()
	dedupedObjects, conditions, err := controller.NormalizeTargetObjects(
		app.Spec.Destination.Namespace,
		targetObjects,
		infoProvider,
		func(obj *unstructured.Unstructured) error {
			return resourceTracking.SetAppInstance(obj, appLabelKey, app.Name, app.Spec.Destination.Namespace, v1alpha1.TrackingMethodLabel, installationID)
		},
	)
	if err != nil {
		return nil, fmt.Errorf("error normalizing target objects: %w", err)
	}

	// Collect duplicate warnings
	for _, condition := range conditions {
		warnings = append(warnings, condition.Message)
	}

	return &TemplateResult{
		Objects:               dedupedObjects,
		Warnings:              warnings,
		SourcesProcessed:      len(requests),
		ApplicationsProcessed: 1,
		Applications: []ApplicationResult{{
			Name:             app.Name,
			Namespace:        app.Spec.Destination.Namespace,
			Objects:          dedupedObjects,
			Warnings:         warnings,
			SourcesProcessed: len(requests),
		}},
	}, nil
}

// readManifest reads a manifest from a file, or from stdin when path is "-".
func readManifest(filePath string) ([]byte, error) {
	var data []byte
	var err error

	if filePath == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(filePath)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read application file: %w", err)
	}

	return data, nil
}

func parseApplication(data []byte) (*v1alpha1.Application, error) {
	var app v1alpha1.Application
	if err := yaml.Unmarshal(data, &app); err != nil {
		return nil, fmt.Errorf("failed to parse Application YAML: %w", err)
	}

	if app.Kind != "Application" {
		return nil, fmt.Errorf("expected kind 'Application', got '%s'", app.Kind)
	}

	return &app, nil
}

func parseApplicationSet(data []byte) (*v1alpha1.ApplicationSet, error) {
	var appSet v1alpha1.ApplicationSet
	if err := yaml.Unmarshal(data, &appSet); err != nil {
		return nil, fmt.Errorf("failed to parse ApplicationSet YAML: %w", err)
	}

	if appSet.Kind != "ApplicationSet" {
		return nil, fmt.Errorf("expected kind 'ApplicationSet', got '%s'", appSet.Kind)
	}

	return &appSet, nil
}

func repoRootOrDefault(repoRoot string) string {
	if repoRoot == "" {
		return "."
	}
	return repoRoot
}

func buildRequestsFromApplication(app *v1alpha1.Application) ([]*apiclient.ManifestRequest, error) {
	sources := app.Spec.GetSources()
	if len(sources) == 0 {
		return nil, fmt.Errorf("no sources found in application spec")
	}

	var requests []*apiclient.ManifestRequest

	for i, source := range sources {
		if source.RepoURL == "" {
			return nil, fmt.Errorf("source[%d].repoURL is required", i)
		}

		// Handle remote Helm charts by downloading them to a temporary directory
		modifiedSource := sources[i]
		if source.IsHelm() {
			chartDir, err := downloadHelmChart(source.RepoURL, source.Chart, source.TargetRevision)
			if err != nil {
				return nil, fmt.Errorf("failed to download Helm chart for source[%d]: %w", i, err)
			}

			// Modify the source to point to the local directory
			modifiedSource = sources[i]
			modifiedSource.Path = chartDir
			modifiedSource.Chart = "" // Clear chart field since we're now using a local path
		}

		req := &apiclient.ManifestRequest{
			Repo: &v1alpha1.Repository{
				Repo: source.RepoURL,
			},
			ApplicationSource: &modifiedSource, // Use the potentially modified source
			AppName:           app.Name,
			Namespace:         app.Spec.Destination.Namespace,
			Revision:          source.TargetRevision,
			EnabledSourceTypes: map[string]bool{
				string(v1alpha1.ApplicationSourceTypeHelm):      true,
				string(v1alpha1.ApplicationSourceTypeKustomize): true,
				string(v1alpha1.ApplicationSourceTypeDirectory): true,
			},
			AppLabelKey:        appLabelKey,
			TrackingMethod:     string(v1alpha1.TrackingMethodLabel),
			InstallationID:     installationID,
			ProjectName:        app.Spec.Project,
			HasMultipleSources: len(sources) > 1,
		}

		requests = append(requests, req)
	}

	return requests, nil
}

// downloadHelmChart downloads a remote Helm chart to the XDG cache directory with reproducible naming
func downloadHelmChart(repoURL, chartName, version string) (string, error) {
	// Get XDG cache directory
	cacheDir, err := getCacheDir()
	if err != nil {
		return "", fmt.Errorf("failed to get cache directory: %w", err)
	}

	// Create subdirectory for helm charts
	helmCacheDir := filepath.Join(cacheDir, "local-argocd-renderer")
	if err := os.MkdirAll(helmCacheDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create helm cache directory: %w", err)
	}

	// Generate reproducible filename based on repoURL, chartName, and version
	hashInput := fmt.Sprintf("%s|%s|%s", repoURL, chartName, version)
	hash := sha256.Sum256([]byte(hashInput))
	hashStr := hex.EncodeToString(hash[:])
	chartDir := filepath.Join(helmCacheDir, fmt.Sprintf("chart-%s", hashStr))

	// Check if chart is already cached
	if _, err := os.Stat(chartDir); err == nil {
		return chartDir, nil
	}

	// Download the chart
	args := []string{"pull", fmt.Sprintf("%s/%s", repoURL, chartName)}
	if version != "" {
		args = append(args, "--version", version)
	}
	args = append(args, "--destination", helmCacheDir)
	args = append(args, "--untar")

	cmd := exec.Command("helm", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("helm pull failed: %w\nOutput: %s", err, string(output))
	}

	// Find the extracted chart directory (helm pull creates a directory with the chart name)
	extractedDir := filepath.Join(helmCacheDir, chartName)

	// Rename to our reproducible name
	if err := os.Rename(extractedDir, chartDir); err != nil {
		return "", fmt.Errorf("failed to rename chart directory: %w", err)
	}

	return chartDir, nil
}

// getCacheDir returns the XDG cache directory
func getCacheDir() (string, error) {
	if cacheDir := os.Getenv("XDG_CACHE_HOME"); cacheDir != "" {
		return cacheDir, nil
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(homeDir, ".cache"), nil
}

// resourceInfoProviderStub is a simple implementation of kubeutil.ResourceInfoProvider
// that treats all resources as cluster-scoped (returns false for IsNamespaced)
type resourceInfoProviderStub struct{}

func (r *resourceInfoProviderStub) IsNamespaced(_ schema.GroupKind) (bool, error) {
	return true, nil
}
