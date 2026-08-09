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
every [release](../../releases), xz-compressed:

```bash
curl -fsSLO https://github.com/pkalemba/local-argocd-renderer/releases/latest/download/local-argocd-renderer_vX.Y.Z_linux_amd64.xz
unxz local-argocd-renderer_vX.Y.Z_linux_amd64.xz
chmod +x local-argocd-renderer_vX.Y.Z_linux_amd64
```

A multi-platform container image is published to `ghcr.io/pkalemba/local-argocd-renderer`,
bundling the `helm` and `kustomize` the renderer shells out to:

```bash
docker run --rm -v "$PWD:/repo" ghcr.io/pkalemba/local-argocd-renderer \
  --app examples/directory/app.yaml
```

Both are produced by the release workflow — see [Releasing](#releasing).

### Updating

A released binary can replace itself:

```bash
local-argocd-renderer --check-update   # just report
local-argocd-renderer --self-update    # download, verify, replace
```

The asset is checked against the release's `checksums.txt` before anything is written,
and the file mode is preserved. Set `GITHUB_TOKEN` if you run into GitHub's 60
requests/hour anonymous limit; an update costs about three.

This only works on binaries built from a release tag. Builds from source carry a
`git describe` version like `v1.1.0-3-gabc1234`, which semver reads as a *prerelease* of
`v1.1.0` — older than the tag it was built from — so self-updating one would move it
backwards. Those are refused. The same goes for binaries under `/nix/store` or a Homebrew
Cellar, which belong to the package manager that installed them.

### Why the binary is large

Around 75 MB uncompressed. It links Argo CD's rendering libraries, which pull in
`k8s.io/api`, `k8s.io/kubernetes` and `client-go`; `reposerver/repository` alone — the
package the renderer is built on — accounts for 67 MB of that. Everything else in this
repository adds roughly 7 MB, so there is no meaningful trimming to do short of
reimplementing what Argo CD already does. The release assets are compressed instead,
which takes them to about 15 MB.

### Releasing

Releases are cut by [semantic-release](https://semantic-release.gitbook.io) on every
push to `main`. It reads the commits since the last tag, decides whether a release is
warranted and what the version is, then creates the tag and the GitHub release and runs
the same make targets used locally:

| Commit | Result |
|--------|--------|
| `fix: ...` | patch release |
| `feat: ...` | minor release |
| `feat!: ...` or a `BREAKING CHANGE:` footer | major release |
| anything else (`chore`, `docs`, `refactor`, …) | no release |

**Commits on `main` therefore have to follow
[Conventional Commits](https://www.conventionalcommits.org)** — a commit that does not
is simply treated as not release-worthy, so a fix written without the `fix:` prefix
silently never ships.

A release publishes the cross-compiled binaries with their checksums as release assets,
and pushes the multi-platform image to GHCR as `:vX.Y.Z` and `:latest`. There is no tag
trigger: pushing a `v*` tag by hand does nothing, because a second path to publishing
could only disagree with this one. CI builds the image on every pull request but never
pushes it.

> **Note**: the first push creates the GHCR package as **private**. It has to be switched
> to public once, under the repository's Packages settings, before `docker run` works
> without authenticating.

Dependencies are kept up to date by Renovate (`renovate.json5`), which writes conventional
commit messages so its merged updates cut patch releases. Argo CD, gitops-engine
and the `k8s.io` modules land in a single `argo-cd stack` pull request, because go.mod
mirrors pins that Argo CD does not export — those have to be reconciled against the
release's own go.mod before merging. The `helm` and `kustomize` versions pinned in the
Dockerfile and the CI workflow are tracked too.

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

# Render every Application and ApplicationSet in a tree
./local-argocd-renderer --dir examples/

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
| `--app` | Application or ApplicationSet manifest, `-` for stdin |
| `--dir` | Directory to scan recursively instead of naming a single manifest |
| `--clusters` | File or directory with Argo CD cluster secrets for the cluster generator |
| `--include-applications` | Also emit the Application resources themselves |
| `--include-tests` | Keep the test hooks Helm charts ship, which are dropped by default |
| `--output-dir` | Write `<dir>/<application>/<kind>-<name>.yaml` instead of printing to stdout |
| `--quiet` | Suppress progress output on stderr, including the Argo CD libraries'. Warnings and errors still come through |
| `--version` | Print the version and exit |
| `--check-update` | Report whether a newer release exists, without installing it |
| `--self-update` | Replace this binary with the latest release |

Exactly one of `--app` and `--dir` is required.

### Scanning a directory

`--dir` walks the directory for `.yaml` and `.yml` files, renders every Application and
ApplicationSet it finds — including several per file, separated by `---` — and ignores
documents of any other kind, so a tree of mixed manifests can be pointed at directly.
Hidden directories are skipped. Files are rendered in sorted order, so the same tree
always produces the same output.

Naming a single manifest with `--app` is stricter: a document that is neither an
Application nor an ApplicationSet is an error there, because the file was named
explicitly.

### Chart tests

Charts commonly ship test hooks — a Pod annotated `helm.sh/hook: test` — and
`helm template` renders them like any other manifest. They are dropped by default here,
because they are not part of what the cluster runs and are noise in a diff.

That is the same as setting Argo CD's per-source switch on every Application:

```yaml
spec:
  source:
    helm:
      skipTests: true
```

`--include-tests` hands the decision back to the manifests: the hooks are rendered
unless a source asks for them to be skipped with the field above.

```bash
./local-argocd-renderer --dir apps/ --include-tests
```

Non-Helm sources are unaffected either way — the tests are only skipped once a source has
been identified as Helm, because in Argo CD's model a non-empty `helm:` block is itself
what marks a source as a chart.

### Emitting the Applications themselves

`--include-applications` puts the Application resource ahead of the manifests it renders
to. This is mainly useful for ApplicationSets, where the generated Applications are
resources you would apply in their own right and are otherwise never written down:

```bash
./local-argocd-renderer --dir apps/ --include-applications --output-dir rendered
```

```
rendered/list-one/application-list-one.yaml
rendered/list-one/configmap-one.yaml
rendered/list-two/application-list-two.yaml
rendered/list-two/configmap-two.yaml
```

## Library

```go
import (
    "context"
    "log"

    renderer "github.com/lorenzbischof/local-argocd-renderer"
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

The package lives at the module root and is named `renderer`, hence the named
import above.

`TemplateFromApplication` and `TemplateFromApplicationSet` are available when the
kind is known up front, along with their `...YAML` variants, which take the manifest
as a string. All of them take the same `TemplateOptions`. To only expand an
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

The `kind: List` that `kubectl` wraps a multi-secret export in is unwrapped, and both
`data` and `stringData` are accepted, so the secrets can equally be written by hand as
plain documents — see `examples/appset-clusters/clusters.yaml`. The secret's labels behave as
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
