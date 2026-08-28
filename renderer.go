package renderer

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"

	"sigs.k8s.io/yaml"

	"github.com/argoproj/argo-cd/v3/controller"
	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v3/reposerver/apiclient"
	"github.com/argoproj/argo-cd/v3/reposerver/repository"
	argopath "github.com/argoproj/argo-cd/v3/util/app/path"
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
	// IncludeHelmTests keeps the test hooks charts ship. They are dropped by
	// default, which is the equivalent of setting spec.source.helm.skipTests on
	// every Application; with this set the per-source field decides on its own,
	// the way Argo CD reads it.
	IncludeHelmTests bool
	// HelmCapabilitiesFile points at a YAML file describing the destination
	// cluster — its Kubernetes version and the API versions installed on it —
	// so that the `.Capabilities` guards a chart writes decide here the way they
	// would against that cluster. See HelmCapabilities for the format. Without
	// it Helm renders against its own built-in defaults, as if none of the CRDs
	// a chart looks for existed.
	HelmCapabilitiesFile string
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

// TemplateFromApplicationYAML processes an ArgoCD Application from YAML content.
//
// It takes the same options as every other entry point. It used to take a bare
// repoRoot and build the options itself, which silently discarded the rest of them
// — a caller asking for Helm tests, or for a manifest size cap, got neither.
func TemplateFromApplicationYAML(ctx context.Context, yamlContent string, opts TemplateOptions) (*TemplateResult, error) {
	app, err := parseApplication([]byte(yamlContent))
	if err != nil {
		return nil, fmt.Errorf("error parsing Application CRD: %w", err)
	}

	return renderApplication(ctx, app, opts)
}

// TemplateFromApplicationSetYAML processes an ArgoCD ApplicationSet from YAML
// content. As above, it takes the full options — dropping them here also meant an
// ApplicationSet with a cluster generator never saw ClustersFile, so it rendered
// against the in-cluster entry alone.
func TemplateFromApplicationSetYAML(ctx context.Context, yamlContent string, opts TemplateOptions) (*TemplateResult, error) {
	appSet, err := parseApplicationSet([]byte(yamlContent))
	if err != nil {
		return nil, fmt.Errorf("error parsing ApplicationSet CRD: %w", err)
	}

	return renderApplicationSet(ctx, appSet, opts)
}

// renderApplicationSet expands an ApplicationSet into Applications and renders each
// of them, concatenating the results.
func renderApplicationSet(ctx context.Context, appSet *v1alpha1.ApplicationSet, opts TemplateOptions) (*TemplateResult, error) {
	apps, warnings, err := GenerateApplications(appSet, opts)
	if err != nil {
		return nil, err
	}

	// The generators hand back Applications in whatever order their inputs came out
	// in, and the cluster generator's inputs come from a map — so without this the
	// order of a multi-cluster render changed between runs.
	slices.SortFunc(apps, func(a, b v1alpha1.Application) int {
		return cmp.Or(
			cmp.Compare(a.Namespace, b.Namespace),
			cmp.Compare(a.Name, b.Name),
		)
	})

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
	repoRoot, err := absRepoRoot(opts.RepoRoot)
	if err != nil {
		return nil, err
	}

	caps, err := LoadHelmCapabilities(opts.HelmCapabilitiesFile)
	if err != nil {
		return nil, err
	}

	requests, err := buildRequestsFromApplication(app, repoRoot, caps)
	if err != nil {
		return nil, err
	}

	var allManifests []string
	var warnings []string

	// Process each source
	for sourceIndex, source := range requests {
		q, appPath := source.request, source.appPath

		appSourceType, err := repository.GetAppSourceType(ctx, q.ApplicationSource, appPath, repoRoot, q.AppName, q.EnabledSourceTypes, []string{}, []string{})
		if err != nil {
			return nil, fmt.Errorf("error getting app source type: %w", err)
		}

		// helm template renders a chart's test hooks along with everything else,
		// and they are noise in a diff of what the cluster runs, so they are dropped
		// unless asked for. IncludeHelmTests leaves the source untouched instead, so
		// the per-source spec.source.helm.skipTests field decides.
		//
		// It is set only once the source is known to be Helm: a non-zero helm block
		// is itself what marks a source as Helm, so setting it upfront would retype
		// a Kustomize or directory source.
		if !opts.IncludeHelmTests && appSourceType == v1alpha1.ApplicationSourceTypeHelm {
			if q.ApplicationSource.Helm == nil {
				q.ApplicationSource.Helm = &v1alpha1.ApplicationSourceHelm{}
			}
			q.ApplicationSource.Helm.SkipTests = true
		}

		// Rendering a Kustomize source runs `kustomize edit`, which rewrites
		// kustomization.yaml, so it is pointed at a generated overlay rather than at
		// the checkout itself.
		if appSourceType == v1alpha1.ApplicationSourceTypeKustomize {
			overlayDir, err := kustomizeOverlay(appPath)
			if err != nil {
				return nil, err
			}
			defer os.RemoveAll(overlayDir)

			appPath = overlayDir
		}

		maxSize, err := maxManifestSize(opts.MaxManifestSize)
		if err != nil {
			return nil, err
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
	infoProvider := newResourceScopeProvider(targetObjects)
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

	sortObjects(dedupedObjects)

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

// kustomizeOverlay builds a throwaway kustomization that pulls in appPath as its
// only resource, and returns its directory. The caller removes it.
//
// It goes in the system temp directory, not in the working directory: the working
// directory is the repository being rendered, which is read-only in the container
// image (WORKDIR /repo, USER nobody) and which nobody wants a stray
// kustomize-overlay-* left in when a render is interrupted.
//
// The reference back into the checkout has to stay relative. Kustomize refuses an
// absolute one outright — "new root ... cannot be absolute" — but follows a
// relative path out of its own root happily enough.
func kustomizeOverlay(appPath string) (string, error) {
	absAppPath, err := filepath.Abs(appPath)
	if err != nil {
		return "", fmt.Errorf("error resolving Kustomize source %q: %w", appPath, err)
	}

	overlayDir, err := os.MkdirTemp("", "kustomize-overlay-*")
	if err != nil {
		return "", fmt.Errorf("error creating temp directory for Kustomize overlay: %w", err)
	}

	// MkdirTemp can hand back a path through a symlink — /tmp is one on macOS — and
	// a relative path computed against the link does not survive kustomize resolving
	// it, so resolve before measuring.
	resolvedOverlayDir, err := filepath.EvalSymlinks(overlayDir)
	if err != nil {
		os.RemoveAll(overlayDir)
		return "", fmt.Errorf("error resolving the Kustomize overlay directory: %w", err)
	}

	relPath, err := filepath.Rel(resolvedOverlayDir, absAppPath)
	if err != nil {
		os.RemoveAll(overlayDir)
		return "", fmt.Errorf("error calculating relative path: %w", err)
	}

	kustomization := fmt.Sprintf(`apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- %s
`, relPath)

	if err := os.WriteFile(filepath.Join(overlayDir, "kustomization.yaml"), []byte(kustomization), 0o644); err != nil {
		os.RemoveAll(overlayDir)
		return "", fmt.Errorf("error writing kustomization.yaml: %w", err)
	}

	return overlayDir, nil
}

// sortObjects puts the rendered manifests in a stable order.
//
// NormalizeTargetObjects collects its result by ranging over a map, so it hands
// back a different order on every run. That makes `render | git diff` churn for no
// reason, which defeats the point of rendering locally in the first place.
func sortObjects(objects []*unstructured.Unstructured) {
	slices.SortFunc(objects, func(a, b *unstructured.Unstructured) int {
		return cmp.Or(
			cmp.Compare(a.GetAPIVersion(), b.GetAPIVersion()),
			cmp.Compare(a.GetKind(), b.GetKind()),
			cmp.Compare(a.GetNamespace(), b.GetNamespace()),
			cmp.Compare(a.GetName(), b.GetName()),
		)
	})
}

// WriteManifests writes the rendered objects as a YAML stream. The CLI and the
// golden tests both go through here, so what the tests compare against is what the
// CLI actually prints.
func WriteManifests(w io.Writer, result *TemplateResult) error {
	if _, err := fmt.Fprintf(w, "# Generated %d manifests\n---\n", len(result.Objects)); err != nil {
		return err
	}

	for i, object := range result.Objects {
		if i > 0 {
			if _, err := fmt.Fprintln(w, "---"); err != nil {
				return err
			}
		}

		manifest, err := yaml.Marshal(object.Object)
		if err != nil {
			return fmt.Errorf("failed to marshal %s %q: %w", object.GetKind(), object.GetName(), err)
		}
		if _, err := w.Write(manifest); err != nil {
			return err
		}
	}

	return nil
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

	slices.Sort(files)

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

// defaultMaxManifestSize is the repo-server's own default for the combined size of
// a directory source's manifests — argocd-repo-server's
// --max-combined-directory-manifests-size, which defaults to 10M.
const defaultMaxManifestSize = "10M"

// maxManifestSize parses the configured cap, or falls back to the repo-server's.
// It used to go through resource.MustParse, so a caller's typo took the process
// down with a panic from inside a Kubernetes helper rather than being reported.
func maxManifestSize(configured string) (resource.Quantity, error) {
	if configured == "" {
		return resource.MustParse(defaultMaxManifestSize), nil
	}

	size, err := resource.ParseQuantity(configured)
	if err != nil {
		return resource.Quantity{}, fmt.Errorf("invalid MaxManifestSize %q: %w", configured, err)
	}

	return size, nil
}

func repoRootOrDefault(repoRoot string) string {
	if repoRoot == "" {
		return "."
	}
	return repoRoot
}

// absRepoRoot resolves the checkout root to an absolute path, which is what the
// repo-server works in throughout. argopath.Path's containment check compares the
// joined path against the root as a string prefix, and a relative root like "."
// never matches one — so every path would be reported as outside the repo.
func absRepoRoot(repoRoot string) (string, error) {
	absolute, err := filepath.Abs(repoRootOrDefault(repoRoot))
	if err != nil {
		return "", fmt.Errorf("failed to resolve RepoRoot %q: %w", repoRoot, err)
	}

	return absolute, nil
}

// sourceRequest pairs a manifest request with the directory the source was
// resolved to on disk.
type sourceRequest struct {
	request *apiclient.ManifestRequest
	// appPath is where the source's files actually live: spec.source.path resolved
	// against the repo root, or the cache directory for a Helm chart pulled from a
	// registry — which is outside the repo entirely and so is not resolved against
	// it.
	appPath string
}

func buildRequestsFromApplication(app *v1alpha1.Application, repoRoot string, caps *HelmCapabilities) ([]sourceRequest, error) {
	sources := app.Spec.GetSources()
	if len(sources) == 0 {
		return nil, fmt.Errorf("no sources found in application spec")
	}

	var requests []sourceRequest

	for i, source := range sources {
		if source.RepoURL == "" {
			return nil, fmt.Errorf("source[%d].repoURL is required", i)
		}

		// Handle remote Helm charts by downloading them to a temporary directory
		modifiedSource := sources[i]
		var appPath string
		if source.IsHelm() {
			chartDir, err := downloadHelmChart(source.RepoURL, source.Chart, source.TargetRevision)
			if err != nil {
				return nil, fmt.Errorf("failed to download Helm chart for source[%d]: %w", i, err)
			}

			// Modify the source to point to the local directory
			modifiedSource = sources[i]
			modifiedSource.Path = chartDir
			modifiedSource.Chart = "" // Clear chart field since we're now using a local path
			appPath = chartDir
		} else {
			// argopath.Path is what the repo-server resolves a source's path with:
			// it joins the path onto the checkout root and refuses one that is
			// absolute, escapes the root, or is not a directory. Using the path from
			// the manifest as-is meant it was read relative to the process's working
			// directory instead, so RepoRoot only ever worked by coincidence — when
			// the caller happened to already be standing in the repo.
			resolved, err := argopath.Path(repoRoot, source.Path)
			if err != nil {
				return nil, fmt.Errorf("source[%d]: %w", i, err)
			}
			appPath = resolved
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

		// The repo-server reads these off the destination cluster; there is no
		// cluster here, so they come from the file instead.
		caps.applyTo(req)

		requests = append(requests, sourceRequest{request: req, appPath: appPath})
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
	cmd := exec.Command("helm", helmPullArgs(repoURL, chartName, version, helmCacheDir)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("helm pull failed: %w\nOutput: %s", err, string(output))
	}

	// --untar extracts into a directory named after the chart, which for an OCI
	// reference is the last segment of a path that may have several.
	extractedDir := filepath.Join(helmCacheDir, path.Base(chartName))

	// Rename to our reproducible name
	if err := os.Rename(extractedDir, chartDir); err != nil {
		return "", fmt.Errorf("failed to rename chart directory: %w", err)
	}

	return chartDir, nil
}

// helmPullArgs builds the `helm pull` invocation for a chart, mirroring the two
// forms Argo CD's own Helm client uses (util/helm/cmd.go).
//
// A classic HTTP repository is resolved through its index.yaml, which is what
// --repo asks helm to do. Joining the repository URL and the chart name into a
// single argument instead makes helm read it as a literal archive URL, so the
// pull only ever succeeds for a chart that happens to be served at exactly that
// path — which is not how chart repositories are laid out.
//
// An OCI registry has no index, so there the reference *is* the address, and
// helm rejects --repo alongside it.
func helmPullArgs(repoURL, chartName, version, destination string) []string {
	args := []string{"pull"}

	if registry, isOCI := strings.CutPrefix(repoURL, "oci://"); isOCI {
		args = append(args, fmt.Sprintf("oci://%s/%s", strings.TrimSuffix(registry, "/"), chartName))
	} else {
		args = append(args, chartName, "--repo", repoURL)
	}

	if version != "" {
		args = append(args, "--version", version)
	}

	return append(args, "--destination", destination, "--untar")
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
