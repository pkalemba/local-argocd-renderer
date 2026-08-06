# Local Argo CD Renderer

A standalone tool that renders Argo CD applications locally without requiring a running Argo CD server. This addresses the common need for debugging and studying potential Kubernetes manifests during development.

**Solves**: The problem described in [argoproj/argo-cd#11722](https://github.com/argoproj/argo-cd/issues/11722) and [argoproj/argo-cd#11129](https://github.com/argoproj/argo-cd/issues/11129) where developers need to render Argo CD applications locally for testing and debugging purposes.

## Problem Statement

When working with large Argo CD installations with frequent chart changes, developers often need to:
- Debug potential manifests before deployment
- Study the rendered Kubernetes resources
- Validate applications locally without server access
- Test changes to Helm charts, Kustomize configurations, or plain manifests

The official Argo CD CLI requires a server connection (`argocd app manifests` fails with "Argo CD server address unspecified"), making local development and testing difficult.

## Features

- **🚀 No Server Required**: Render applications completely offline
- **🎯 Full Source Type Support**: 
  - Helm charts with values, parameters, and custom options
  - Kustomize applications with overlays and patches
  - Plain YAML/JSON manifest directories
- **🧬 ApplicationSets**: Expand an ApplicationSet with the upstream generators and render every Application it produces
- **🔧 CLI Tool**: Simple command-line interface matching Argo CD patterns
- **📚 Library API**: Go package for integration into other tools

## Installation

Pre-built binaries for Linux, macOS and Windows on amd64/arm64 are attached to
every [release](../../releases), and a multi-platform container image is published
to `ghcr.io/pkalemba/local-argocd-renderer`:

```bash
docker run --rm -v "$PWD:/repo" ghcr.io/pkalemba/local-argocd-renderer \
  --app examples/directory/app.yaml
```

The image bundles `helm` and `kustomize`, which the renderer shells out to.

To build from source:

```bash
make build          # binary for the current platform
make dist           # cross-compiled binaries for every platform in dist/
make image          # multi-platform container image via docker buildx
make test           # unit and golden tests
```

## Usage

### CLI

```bash
# Build the CLI
go build ./cmd/local-argocd-renderer

# Run the CLI
./local-argocd-renderer --app examples/directory/app.yaml

# ApplicationSets work the same way
./local-argocd-renderer --app examples/appset-list/appset.yaml

# Feed the cluster generator with local cluster secrets
./local-argocd-renderer --app examples/appset-clusters/appset.yaml \
  --clusters examples/appset-clusters/clusters.yaml

# Split the output into one file per object, grouped per Application
./local-argocd-renderer --app examples/appset-list/appset.yaml --output-dir rendered

# Or pipe from stdin
cat examples/directory/app.yaml | ./local-argocd-renderer --app -
```

| Flag | Description |
|------|-------------|
| `--app` | Application or ApplicationSet manifest, `-` for stdin (required) |
| `--clusters` | File or directory with Argo CD cluster secrets for the cluster generator |
| `--output-dir` | Write `<dir>/<application>/<kind>-<name>.yaml` instead of printing to stdout |
| `--quiet` | Suppress progress output on stderr |
| `--version` | Print the version and exit |

## Library

```go
import (
    "context"
    "log"

    "github.com/lorenzbischof/local-argocd-renderer/pkg/renderer"
)

ctx := context.Background()

// Template accepts both Applications and ApplicationSets and picks the right path
// based on the manifest's kind.
result, err := renderer.Template(ctx, renderer.TemplateOptions{
    ApplicationFile: "my-app.yaml",
    RepoRoot:        ".",
})
if err != nil {
    log.Fatal(err)
}

// Process result.Objects
```

`TemplateFromApplication` and `TemplateFromApplicationSet` are available when the
kind is known up front, along with their `...YAML` variants. To only expand an
ApplicationSet into Applications without rendering them, use
`renderer.GenerateApplications`.

> **Note**: this module is meant to be built and run, not imported. Its `go.mod`
> carries the `replace` directives Argo CD needs but does not export (gitops-engine
> and the `k8s.io` staging modules), and `replace` does not apply to importers, so
> `go get`ting this package will not resolve.

## ApplicationSets

An ApplicationSet is expanded by the upstream ApplicationSet controller code
(`applicationset/controllers/template`), so generator semantics, `goTemplate`,
`templatePatch`, selectors and nested generators behave exactly as they do in a
cluster. Only the places where the controller talks to the outside world are
replaced with local equivalents:

- The Kubernetes reads are served from an in-memory client, seeded with the
  `AppProject` referenced by the template and with the cluster secrets passed via
  `--clusters`.
- The Git generator reads from the local checkout under `RepoRoot` instead of from
  a repo-server, so `repoURL` and `revision` are ignored the same way they are for
  a plain Application.

| Generator | Supported |
|-----------|-----------|
| `list` | ✅ |
| `git` (`directories` and `files`) | ✅ reads the local checkout |
| `clusters` | ✅ reads the cluster secrets given via `--clusters` |
| `matrix`, `merge` | ✅ with supported child generators |
| `scmProvider`, `pullRequest`, `clusterDecisionResource`, `plugin` | ❌ queries a remote service |

Unsupported generators fail with an explicit error rather than being silently
skipped.

### Clusters

The cluster generator does not talk to the Argo CD API: in a cluster it lists the
`Secret`s labelled `argocd.argoproj.io/secret-type: cluster` in the Argo CD
namespace. The same secrets can be handed to the renderer in a local YAML file (or a
directory of them), which is all the generator needs:

```bash
kubectl -n argocd get secret -l argocd.argoproj.io/secret-type=cluster -o yaml > clusters.yaml
local-argocd-renderer --app appset.yaml --clusters clusters.yaml
```

Both `data` and `stringData` are accepted, so the secrets can also be written by
hand — see `examples/appset-clusters/clusters.yaml`. The secret's labels behave as
they do in a cluster: `clusters.selector` matches on them, and they are exposed to
the template as `{{ .metadata.labels.<key> }}` (or `{{metadata.labels.<key>}}`
without `goTemplate`).

`examples/appset-clusters-helm` shows the usual pattern of deriving the Helm values of
each generated Application from the cluster's labels:

```yaml
generators:
  - clusters:
      selector:
        matchExpressions:
          - key: cluster.com/family
            operator: Exists
      values:
        family: '{{ index .metadata.labels "cluster.com/family" }}'
template:
  spec:
    source:
      helm:
        values: |
          {{- $clusterFamily := index .metadata.labels "cluster.com/family" }}
          {{- $internalClusterDomain := index .metadata.labels "cluster.com/internal-domain" }}
          istio-expose:
            family: {{ .values.family }}
            domain:
            {{- if eq $clusterFamily "int" }}
              name: "test.{{ $internalClusterDomain }}"
            {{- end }}
```

Without `--clusters` the generator still reports the `in-cluster` entry, exactly as
it does on a fresh Argo CD install, and the render carries a warning saying so.

## Examples

The `examples/` directory contains sample applications and ApplicationSets.

## Comparison with Argo CD CLI

| Feature | `argocd app manifests` | `local-argocd-renderer` |
|---------|------------------------|-------------------------|
| Server Required | ❌ Yes | ✅ No |
| Offline Usage | ❌ No | ✅ Yes |
| Local Development | ❌ Limited | ✅ Full |
| CI/CD Friendly | ❌ No | ✅ Yes |
| Setup Complexity | 🟡 Medium | ✅ Simple |

## Limitations

- **Local repositories only**: Remote Git repositories must be cloned first
- **No server-side plugins**: Custom Argo CD plugins are not currently supported
- **Remote ApplicationSet generators**: `scmProvider`, `pullRequest`, `clusterDecisionResource` and `plugin` query a remote service and are not supported
- **Simplified validation**: Some advanced Argo CD validation rules are not applied
- **No diff capabilities**: Only renders manifests, doesn't compare with cluster state

## License

Apache License 2.0 - see [LICENSE](LICENSE) for details.
