package renderer

import (
	"fmt"
	"maps"
	"strings"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/argoproj/argo-cd/v3/applicationset/controllers/template"
	"github.com/argoproj/argo-cd/v3/applicationset/generators"
	"github.com/argoproj/argo-cd/v3/applicationset/utils"
	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

// GenerateApplications expands an ApplicationSet into the Applications it would
// produce on a cluster, along with any warnings about the local expansion. The
// expansion is done by the upstream ApplicationSet controller code; only the pieces
// that would talk to the Kubernetes API server or to a repo-server are swapped for
// local equivalents.
func GenerateApplications(appSet *v1alpha1.ApplicationSet, opts TemplateOptions) ([]v1alpha1.Application, []string, error) {
	clusterSecrets, err := clusterSecretsFor(appSet, opts)
	if err != nil {
		return nil, nil, err
	}

	k8sClient, err := localClient(appSet, clusterSecrets)
	if err != nil {
		return nil, nil, err
	}

	// The cluster generator always reports the in-cluster entry, even without any
	// cluster secret, so a render without --clusters silently covers fewer clusters
	// than the real controller would.
	var clustersUsed atomic.Bool

	apps, _, err := template.GenerateApplications(
		log.NewEntry(log.StandardLogger()),
		*appSet,
		localGenerators(localGeneratorOptions{
			repoRoot:            repoRootOrDefault(opts.RepoRoot),
			controllerNamespace: appSet.Namespace,
			k8sClient:           k8sClient,
			clustersUsed:        &clustersUsed,
		}),
		&utils.Render{},
		k8sClient,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("error generating applications from ApplicationSet: %w", err)
	}

	var warnings []string
	if clustersUsed.Load() && opts.ClustersFile == "" {
		warnings = append(warnings, "the cluster generator only saw the in-cluster entry: pass the Argo CD cluster secrets via ClustersFile to render the registered clusters as well")
	}

	return apps, warnings, nil
}

func clusterSecretsFor(appSet *v1alpha1.ApplicationSet, opts TemplateOptions) ([]corev1.Secret, error) {
	if opts.ClustersFile == "" {
		return nil, nil
	}

	return loadClusterSecrets(opts.ClustersFile, appSet.Namespace)
}

type localGeneratorOptions struct {
	repoRoot            string
	controllerNamespace string
	k8sClient           client.Client
	clustersUsed        *atomic.Bool
}

// localGenerators mirrors generators.GetGenerators for an offline render: the List,
// Git and Clusters generators are the upstream ones fed from local inputs, and
// everything that has to reach out over the network reports an error instead.
func localGenerators(opts localGeneratorOptions) map[string]generators.Generator {
	clusterGen := generators.NewClusterGenerator(opts.k8sClient, opts.controllerNamespace)

	terminalGenerators := map[string]generators.Generator{
		"List":                    generators.NewListGenerator(),
		"Git":                     generators.NewGitGenerator(newLocalRepos(opts.repoRoot), opts.controllerNamespace),
		"Clusters":                &observedGenerator{Generator: clusterGen, used: opts.clustersUsed},
		"SCMProvider":             unsupportedGeneratorNamed("scmProvider"),
		"ClusterDecisionResource": unsupportedGeneratorNamed("clusterDecisionResource"),
		"PullRequest":             unsupportedGeneratorNamed("pullRequest"),
		"Plugin":                  unsupportedGeneratorNamed("plugin"),
	}

	nestedGenerators := maps.Clone(terminalGenerators)
	nestedGenerators["Matrix"] = generators.NewMatrixGenerator(terminalGenerators)
	nestedGenerators["Merge"] = generators.NewMergeGenerator(terminalGenerators)

	topLevelGenerators := maps.Clone(terminalGenerators)
	topLevelGenerators["Matrix"] = generators.NewMatrixGenerator(nestedGenerators)
	topLevelGenerators["Merge"] = generators.NewMergeGenerator(nestedGenerators)

	return topLevelGenerators
}

// localClient provides the Kubernetes reads the generators perform: the AppProject
// the Git generator resolves to decide whether commit signatures have to be
// verified, and the cluster secrets the Clusters generator selects on.
func localClient(appSet *v1alpha1.ApplicationSet, clusterSecrets []corev1.Secret) (client.Client, error) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("error building scheme: %w", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("error building scheme: %w", err)
	}

	builder := fake.NewClientBuilder().WithScheme(scheme)

	// A templated project name is resolved per parameter set, so there is nothing to
	// look up here; the generators skip the lookup in that case as well.
	if project := appSet.Spec.Template.Spec.Project; project != "" && !strings.Contains(project, "{{") {
		builder = builder.WithObjects(&v1alpha1.AppProject{
			ObjectMeta: metav1.ObjectMeta{Name: project, Namespace: appSet.Namespace},
		})
	}

	for i := range clusterSecrets {
		builder = builder.WithObjects(&clusterSecrets[i])
	}

	return builder.Build(), nil
}

// observedGenerator records that a generator was reached, so that the caller can
// report on how it was resolved locally.
type observedGenerator struct {
	generators.Generator
	used *atomic.Bool
}

func (g *observedGenerator) GenerateParams(appSetGenerator *v1alpha1.ApplicationSetGenerator, appSet *v1alpha1.ApplicationSet, c client.Client) ([]map[string]any, error) {
	g.used.Store(true)

	return g.Generator.GenerateParams(appSetGenerator, appSet, c)
}

// unsupportedGenerator stands in for generators that have to reach a live service.
// Registering it keeps generators.Transform on its regular error path instead of
// dereferencing a missing entry in the generator map.
type unsupportedGenerator struct {
	name string
}

func unsupportedGeneratorNamed(name string) generators.Generator {
	return &unsupportedGenerator{name: name}
}

func (g *unsupportedGenerator) GenerateParams(_ *v1alpha1.ApplicationSetGenerator, _ *v1alpha1.ApplicationSet, _ client.Client) ([]map[string]any, error) {
	return nil, fmt.Errorf("the %s generator queries a remote service and cannot be rendered locally", g.name)
}

func (g *unsupportedGenerator) GetRequeueAfter(_ *v1alpha1.ApplicationSetGenerator) time.Duration {
	return generators.NoRequeueAfter
}

func (g *unsupportedGenerator) GetTemplate(_ *v1alpha1.ApplicationSetGenerator) *v1alpha1.ApplicationSetTemplate {
	return &v1alpha1.ApplicationSetTemplate{}
}
