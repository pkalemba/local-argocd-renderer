package e2e

import (
	"context"
	"os"
	"path/filepath"
	"sigs.k8s.io/yaml"
	"testing"
	"time"

	applicationV1Alpha1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"sigs.k8s.io/e2e-framework/third_party/helm"
)

var currentDir, _ = os.Getwd()

func TestArgoCD(t *testing.T) {
	feature := features.
		New("ArgoCD server").
		Setup(func(ctx context.Context, t *testing.T, config *envconf.Config) context.Context {
			err := AddResourcesToScheme(config)
			require.NoError(t, err)

			helmMgr := helm.New(config.KubeconfigFile())
			argoClient := NewResourceManager(config)

			err = helmMgr.RunRepo(helm.WithArgs(
				"add",
				"argo",
				"https://argoproj.github.io/argo-helm",
			))
			require.NoError(t, err)

			err = helmMgr.RunInstall(
				helm.WithName("argo-cd"),
				helm.WithNamespace(argocdNamespace),
				helm.WithReleaseName("argo/argo-cd"),
				// Chart 10.3.0 ships Argo CD v3.5.0, the version whose libraries
				// the renderer links. The pin was 5.34.1 — Argo CD 2.7 — so the
				// test compared this renderer's output against a control plane
				// three majors behind it.
				helm.WithVersion("10.3.0"),
			)
			require.NoError(t, err)

			argoAppSpec, err := os.ReadFile(filepath.Join(currentDir, "..", "examples", "directory", "app.yaml"))
			require.NoError(t, err)

			argoApp, err := GetArgoApplicationFromYAML(argoAppSpec)
			require.NoError(t, err)

			err = argoClient.CreateApplicationWithContext(ctx, argoApp)
			require.NoError(t, err)

			return ctx
		}).
		Assess(
			"Manifest output",
			func(ctx context.Context, t *testing.T, config *envconf.Config) context.Context {
				app := &applicationV1Alpha1.Application{ObjectMeta: metav1.ObjectMeta{
					Name:      "directory-app",
					Namespace: argocdNamespace,
				}}

				var isAppHealthyAndSynced = func(object k8s.Object) bool {
					argoApp := object.(*applicationV1Alpha1.Application)

					return string(argoApp.Status.Health.Status) == "Healthy" &&
						string(argoApp.Status.Sync.Status) == "Synced"
				}

				err := wait.For(
					conditions.New(config.Client().Resources()).ResourceMatch(app, isAppHealthyAndSynced),
					wait.WithTimeout(time.Minute*5),
				)
				assert.NoError(t, err, "Error waiting for ArgoCD app to sync")

				return ctx
			}).
		Teardown(func(ctx context.Context, t *testing.T, config *envconf.Config) context.Context {
			helmClient := helm.New(config.KubeconfigFile())
			err := helmClient.RunRepo(helm.WithArgs("remove", "argo"))
			require.NoError(t, err)

			return ctx
		}).Feature()

	testEnv.Test(t, feature)
}

type ResourceManager struct {
	k8sConfig *envconf.Config
}

func (r *ResourceManager) GetApplicationWithContext(
	ctx context.Context,
	name string,
	namespace string,
) (*applicationV1Alpha1.Application, error) {
	app := &applicationV1Alpha1.Application{}
	err := r.k8sConfig.Client().Resources().Get(ctx, name, namespace, app)
	if err != nil {
		return &applicationV1Alpha1.Application{}, err
	}
	return app, nil
}

func (r *ResourceManager) CreateApplicationWithContext(
	ctx context.Context,
	obj *applicationV1Alpha1.Application,
) error {
	return r.k8sConfig.Client().Resources().Create(ctx, obj)
}

func NewResourceManager(config *envconf.Config) *ResourceManager {
	return &ResourceManager{k8sConfig: config}
}

func AddResourcesToScheme(config *envconf.Config) error {
	scheme := config.Client().Resources().GetScheme()
	return applicationV1Alpha1.AddToScheme(scheme)
}

func GetArgoApplicationFromYAML(fileData []byte) (*applicationV1Alpha1.Application, error) {
	app := &applicationV1Alpha1.Application{}
	jsonData, err := yaml.YAMLToJSON(fileData)
	if err != nil {
		return &applicationV1Alpha1.Application{}, err
	}

	err = yaml.Unmarshal(jsonData, app)
	if err != nil {
		return &applicationV1Alpha1.Application{}, err
	}
	app.SetNamespace(argocdNamespace)

	return app, nil
}
