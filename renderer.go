package renderer

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"

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
	// ApplicationDir is scanned recursively for manifests instead, rendering every
	// Application and ApplicationSet it finds and ignoring everything else. It is
	// mutually exclusive with ApplicationFile.
	ApplicationDir  string
	RepoRoot        string
	MaxManifestSize string
	// ClustersFile points at a file or directory holding Argo CD cluster secrets.
	// Without it the ApplicationSet cluster generator only sees the in-cluster entry.
	ClustersFile string
	// IncludeApplications emits the Application resource itself alongside the
	// manifests it renders to, so that the Applications an ApplicationSet generates
	// come out next to their contents.
	IncludeApplications bool
	// SkipHelmTests passes --skip-tests to every Helm source, dropping the test
	// hooks charts ship. It is the equivalent of setting spec.source.helm.skipTests
	// on each Application, which Argo CD honours on its own.
	SkipHelmTests bool
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

// Template processes the ArgoCD Applications and ApplicationSets named by opts,
// either the single manifest in ApplicationFile or everything under ApplicationDir.
// The kind is taken from the manifests themselves.
func Template(ctx context.Context, opts TemplateOptions) (*TemplateResult, error) {
	if opts.ApplicationDir != "" {
		if opts.ApplicationFile != "" {
			return nil, fmt.Errorf("ApplicationFile and ApplicationDir are mutually exclusive")
		}
		return TemplateFromDirectory(ctx, opts)
	}

	data, err := readManifest(opts.ApplicationFile)
	if err != nil {
		return nil, err
	}

	return TemplateFromYAML(ctx, string(data), opts)
}

// TemplateFromYAML processes the ArgoCD Applications and ApplicationSets in YAML
// content, which may hold several documents. A document of any other kind is an
// error here, because the content was named explicitly.
func TemplateFromYAML(ctx context.Context, yamlContent string, opts TemplateOptions) (*TemplateResult, error) {
	return templateDocuments(ctx, []byte(yamlContent), opts, false)
}

// TemplateFromDirectory renders every Application and ApplicationSet in the
// manifests below opts.ApplicationDir. Documents of any other kind are skipped, so
// a tree of mixed manifests can be pointed at directly.
func TemplateFromDirectory(ctx context.Context, opts TemplateOptions) (*TemplateResult, error) {
	files, err := manifestFiles(opts.ApplicationDir)
	if err != nil {
		return nil, err
	}

	result := &TemplateResult{}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("failed to read %q: %w", file, err)
		}

		fileResult, err := templateDocuments(ctx, data, opts, true)
		if err != nil {
			return nil, fmt.Errorf("error rendering %q: %w", file, err)
		}

		result.merge(fileResult)
	}

	return result, nil
}

// templateDocuments renders every Application and ApplicationSet document in data.
// With skipOtherKinds set, documents of another kind are ignored rather than
// rejected.
func templateDocuments(ctx context.Context, data []byte, opts TemplateOptions, skipOtherKinds bool) (*TemplateResult, error) {
	documents, err := splitDocuments(data)
	if err != nil {
		return nil, err
	}

	result := &TemplateResult{}
	for _, document := range documents {
		var typeMeta metav1.TypeMeta
		if err := yaml.Unmarshal(document, &typeMeta); err != nil {
			if skipOtherKinds {
				// Not every YAML file in a tree of manifests is a Kubernetes object.
				continue
			}
			return nil, fmt.Errorf("failed to parse YAML: %w", err)
		}

		var documentResult *TemplateResult
		switch typeMeta.Kind {
		case "Application":
			app, err := parseApplication(document)
			if err != nil {
				return nil, err
			}
			documentResult, err = renderApplication(ctx, app, opts)
			if err != nil {
				return nil, err
			}
		case "ApplicationSet":
			appSet, err := parseApplicationSet(document)
			if err != nil {
				return nil, err
			}
			documentResult, err = renderApplicationSet(ctx, appSet, opts)
			if err != nil {
				return nil, err
			}
		default:
			if skipOtherKinds {
				continue
			}
			return nil, fmt.Errorf("expected kind 'Application' or 'ApplicationSet', got '%s'", typeMeta.Kind)
		}

		result.merge(documentResult)
	}

	return result, nil
}

// merge appends the contents of other, keeping the per-Application grouping.
func (r *TemplateResult) merge(other *TemplateResult) {
	r.Objects = append(r.Objects, other.Objects...)
	r.Warnings = append(r.Warnings, other.Warnings...)
	r.SourcesProcessed += other.SourcesProcessed
	r.ApplicationsProcessed += other.ApplicationsProcessed
	r.Applications = append(r.Applications, other.Applications...)
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

		// helm template renders a chart's test hooks along with everything else.
		// Argo CD has a per-source switch for that, and this turns it on for every
		// chart without having to edit each Application.
		//
		// It is set only once the source is known to be Helm: a non-zero helm block
		// is itself what marks a source as Helm, so setting it upfront would retype
		// a Kustomize or directory source.
		if opts.SkipHelmTests && appSourceType == v1alpha1.ApplicationSourceTypeHelm {
			if q.ApplicationSource.Helm == nil {
				q.ApplicationSource.Helm = &v1alpha1.ApplicationSourceHelm{}
			}
			q.ApplicationSource.Helm.SkipTests = true
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

	// The Application goes first, ahead of what it renders to. It is deliberately
	// added after normalization: it lives in the Argo CD namespace, not in the
	// destination namespace the rendered manifests are moved into.
	if opts.IncludeApplications {
		manifest, err := applicationManifest(app)
		if err != nil {
			return nil, err
		}
		dedupedObjects = append([]*unstructured.Unstructured{manifest}, dedupedObjects...)
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

// applicationManifest turns an Application into the resource you would apply to a
// cluster. Applications generated by an ApplicationSet carry no apiVersion or kind,
// and none of them carry a meaningful status, so both are set straight.
func applicationManifest(app *v1alpha1.Application) (*unstructured.Unstructured, error) {
	clean := app.DeepCopy()
	clean.TypeMeta = metav1.TypeMeta{
		APIVersion: v1alpha1.SchemeGroupVersion.String(),
		Kind:       "Application",
	}
	clean.Status = v1alpha1.ApplicationStatus{}
	clean.ManagedFields = nil

	data, err := json.Marshal(clean)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal application %q: %w", app.Name, err)
	}

	var obj unstructured.Unstructured
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, fmt.Errorf("failed to convert application %q: %w", app.Name, err)
	}

	// Empty values the round trip adds back, which only make the output noisier.
	unstructured.RemoveNestedField(obj.Object, "metadata", "creationTimestamp")
	unstructured.RemoveNestedField(obj.Object, "status")

	return &obj, nil
}

// manifestFiles returns the manifests below dir, sorted so that a render of the
// same tree keeps producing the same output. Hidden directories are skipped,
// matching how the repo-server walks a repository.
func manifestFiles(dir string) ([]string, error) {
	var files []string

	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, fnErr error) error {
		if fnErr != nil {
			return fnErr
		}
		if entry.IsDir() {
			// The directory being walked can itself be named ".", or be hidden.
			if path != dir && strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}

		// Only YAML: scanning .json as well would mean reading every package.json
		// and tsconfig.json in a repository, and Applications are not written in
		// JSON. A JSON manifest can still be rendered by naming it with --app.
		switch filepath.Ext(entry.Name()) {
		case ".yaml", ".yml":
			files = append(files, path)
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to scan %q: %w", dir, err)
	}

	sort.Strings(files)

	return files, nil
}

// splitDocuments splits multi-document YAML into its non-empty documents.
func splitDocuments(data []byte) ([][]byte, error) {
	var documents [][]byte

	reader := k8syaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	for {
		document, err := reader.Read()
		if err == io.EOF {
			return documents, nil
		}
		if err != nil {
			return nil, fmt.Errorf("failed to split YAML documents: %w", err)
		}

		// A document holding only comments or whitespace parses as null, which is
		// neither an Application nor an error.
		if len(bytes.TrimSpace(document)) == 0 {
			continue
		}
		var probe any
		if err := yaml.Unmarshal(document, &probe); err == nil && probe == nil {
			continue
		}

		documents = append(documents, document)
	}
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
