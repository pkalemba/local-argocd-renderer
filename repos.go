package renderer

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/argoproj/argo-cd/v3/applicationset/services"
	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v3/util/git"
)

// localRepos serves the ApplicationSet Git generator from a local checkout instead
// of from a repo-server. Just like the Application renderer, every repoURL and
// revision resolves to RepoRoot; nothing is fetched.
//
// It mirrors what reposerver's Service.GetGitFiles and Service.GetGitDirectories do
// once a revision has been checked out.
type localRepos struct {
	root string
}

var _ services.Repos = (*localRepos)(nil)

func newLocalRepos(repoRoot string) services.Repos {
	return &localRepos{root: repoRoot}
}

// GetFiles returns the contents of the files matching pattern, keyed by their path
// relative to the repository root.
func (r *localRepos) GetFiles(_ context.Context, repoURL, _, _, pattern string, _ bool, _ *v1alpha1.SourceIntegrity) (map[string][]byte, error) {
	root, err := filepath.Abs(r.root)
	if err != nil {
		return nil, fmt.Errorf("error resolving repo root %q: %w", r.root, err)
	}

	// The git client globs the working tree when the new file globbing is enabled,
	// which also works on a checkout that is not a git repository.
	gitClient, err := git.NewClientExt(repoURL, root, git.NopCreds{}, false, false, "", "")
	if err != nil {
		return nil, fmt.Errorf("error creating git client for %q: %w", repoURL, err)
	}

	paths, err := gitClient.LsFiles(pattern, true)
	if err != nil {
		return nil, fmt.Errorf("error listing files matching %q: %w", pattern, err)
	}

	files := make(map[string][]byte, len(paths))
	for _, path := range paths {
		info, err := os.Stat(filepath.Join(root, path))
		if err != nil {
			return nil, fmt.Errorf("error reading %q: %w", path, err)
		}
		if info.IsDir() { // Skip directories: files only
			continue
		}

		contents, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			return nil, fmt.Errorf("error reading %q: %w", path, err)
		}
		files[path] = contents
	}

	return files, nil
}

// GetDirectories returns every directory below the repository root, as paths
// relative to that root. Hidden directories are skipped, matching the repo-server
// default.
func (r *localRepos) GetDirectories(_ context.Context, _, _, _ string, _ bool, _ *v1alpha1.SourceIntegrity) ([]string, error) {
	root, err := filepath.Abs(r.root)
	if err != nil {
		return nil, fmt.Errorf("error resolving repo root %q: %w", r.root, err)
	}

	var paths []string
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, fnErr error) error {
		if fnErr != nil {
			return fmt.Errorf("error walking the file tree: %w", fnErr)
		}
		if !entry.IsDir() { // Skip files: directories only
			return nil
		}
		if strings.HasPrefix(entry.Name(), ".") {
			return filepath.SkipDir // Skip hidden directory
		}

		relativePath, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("error constructing relative repo path: %w", err)
		}
		if relativePath == "." { // Exclude '.' from results
			return nil
		}

		paths = append(paths, relativePath)

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("error walking repo root %q: %w", root, err)
	}

	return paths, nil
}
