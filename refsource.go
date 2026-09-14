package renderer

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v3/util/git"
	utilio "github.com/argoproj/argo-cd/v3/util/io"
)

// refKeyPattern is the character set Argo CD accepts for a sources[].ref, which
// becomes the $name a value file refers to.
var refKeyPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// isRefOnlySource reports whether a source exists purely so that another source
// can read files out of it through $ref. The repo-server refuses to render one
// (Service.GenerateManifest returns an empty response for it), and so must we:
// such a source has no path of its own, and an empty path resolves to the
// checkout root, which would render every manifest lying around the repository.
func isRefOnlySource(source *v1alpha1.ApplicationSource, hasMultipleSources bool) bool {
	return hasMultipleSources && source.IsRef() && source.Path == "" && source.Chart == ""
}

// refTargets maps each "$name" a value file may refer to onto the source that
// declared it, mirroring argo.GetRefSources — which builds the same map from the
// repositories registered with the API server. There is no API server here, so
// the repository comes from the source's own repoURL.
//
// $ref only means anything to an Application with several sources; a single
// source has nothing to refer to.
func refTargets(sources []v1alpha1.ApplicationSource) (map[string]*v1alpha1.RefTarget, error) {
	if len(sources) <= 1 {
		return nil, nil
	}

	targets := map[string]*v1alpha1.RefTarget{}

	for i, source := range sources {
		if !source.IsRef() {
			continue
		}

		if !refKeyPattern.MatchString(source.Ref) {
			return nil, fmt.Errorf("source[%d].ref %q cannot contain any special characters except '_' and '-'", i, source.Ref)
		}

		key := "$" + source.Ref
		if _, duplicate := targets[key]; duplicate {
			return nil, fmt.Errorf("source[%d]: multiple sources had the same 'ref' key %q", i, source.Ref)
		}

		targets[key] = &v1alpha1.RefTarget{
			Repo:           v1alpha1.Repository{Repo: source.RepoURL},
			TargetRevision: source.TargetRevision,
			Chart:          source.Chart,
		}
	}

	return targets, nil
}

// validateRefs rejects a $name that no source declares, before it reaches path
// resolution. Left alone it resolves to nothing at all — the unknown variable
// expands to the empty string, and what is left is read as a path from the
// checkout root — so a typo would quietly render the chart against the wrong
// values file, or against none.
//
// runRepoOperation makes the same checks in the repo-server, but that is Service
// code, and only the package-level GenerateManifests is used here. Like the
// repo-server it leaves a single-source Application alone: $ref means nothing
// there, and a $VAR in a value file is a build environment variable to be
// substituted rather than a source to be resolved.
func validateRefs(source *v1alpha1.ApplicationSource, targets map[string]*v1alpha1.RefTarget, hasMultipleSources bool) error {
	if !hasMultipleSources || source.Helm == nil {
		return nil
	}

	candidates := slices.Clone(source.Helm.ValueFiles)
	for _, fileParam := range source.Helm.FileParameters {
		candidates = append(candidates, fileParam.Path)
	}

	for _, candidate := range candidates {
		if !strings.HasPrefix(candidate, "$") {
			continue
		}

		refVar := strings.Split(candidate, "/")[0]

		target, declared := targets[refVar]
		if !declared {
			if len(targets) == 0 {
				return fmt.Errorf("source referenced %q, but no source has a 'ref' field defined", refVar)
			}
			return fmt.Errorf("source referenced %q, which is not one of the available sources (%s)", refVar, strings.Join(refKeys(targets), ", "))
		}

		if target.Chart != "" {
			return fmt.Errorf("source referenced %q, which has a 'chart' field defined: Helm charts are not supported for 'ref' sources", refVar)
		}
	}

	return nil
}

// refKeys lists the declared $names in a stable order, so that the error naming
// them reads the same on every run.
func refKeys(targets map[string]*v1alpha1.RefTarget) []string {
	keys := make([]string, 0, len(targets))
	for key := range targets {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	return keys
}

// refPaths answers the repo-server's question of where a referenced source was
// checked out to.
//
// The repo-server clones every referenced repository into its own temporary
// directory and hands the resulting map to GenerateManifests. Nothing is cloned
// here: the one checkout that exists is the local repository being rendered, so
// every ref source resolves to it, whichever repoURL it names. Rendering a
// source against a sibling repository's values is not something a local render
// can do without that repository on disk.
type refPaths struct {
	paths map[string]string
}

// newRefPaths points every declared ref source at the local checkout.
func newRefPaths(targets map[string]*v1alpha1.RefTarget, repoRoot string) utilio.TempPaths {
	paths := make(map[string]string, len(targets))
	for _, target := range targets {
		// The repo-server looks the path up by normalized URL, so it has to go in
		// under the same spelling.
		paths[git.NormalizeGitURL(target.Repo.Repo)] = repoRoot
	}

	return &refPaths{paths: paths}
}

func (r *refPaths) Add(key string, value string) {
	r.paths[key] = value
}

func (r *refPaths) GetPath(key string) (string, error) {
	path, ok := r.paths[key]
	if !ok {
		return "", fmt.Errorf("path with key %q not found", key)
	}

	return path, nil
}

func (r *refPaths) GetPathIfExists(key string) string {
	return r.paths[key]
}

func (r *refPaths) GetPaths() map[string]string {
	return r.paths
}
