package renderer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

const mockPullRequestsYAML = `
- number: 1
  title: First
  branch: feature/one_two
  targetBranch: main
  headSHA: 0123456789abcdef
  labels: [preview]
  author: alice
- repo: myorg/other
  number: 2
  branch: feature/elsewhere
  targetBranch: main
  headSHA: abc
- number: 3
  branch: feature/unlabelled
  targetBranch: release
  headSHA: fedcba9876543210
`

func writePullRequests(t *testing.T, data string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "pull-requests.yaml")
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	return path
}

func pullRequestParams(t *testing.T, path string, gen *v1alpha1.PullRequestGenerator, goTemplate bool) []map[string]any {
	t.Helper()

	appSet := &v1alpha1.ApplicationSet{Spec: v1alpha1.ApplicationSetSpec{GoTemplate: goTemplate}}
	params, err := newMockPullRequestGenerator(path, nil).GenerateParams(&v1alpha1.ApplicationSetGenerator{PullRequest: gen}, appSet, nil)
	if err != nil {
		t.Fatalf("GenerateParams failed: %v", err)
	}

	return params
}

func numbers(params []map[string]any) []string {
	var out []string
	for _, p := range params {
		out = append(out, p["number"].(string))
	}

	return out
}

func TestPullRequestParamsMatchUpstream(t *testing.T) {
	path := writePullRequests(t, mockPullRequestsYAML)
	params := pullRequestParams(t, path, &v1alpha1.PullRequestGenerator{
		Github: &v1alpha1.PullRequestGeneratorGithub{Owner: "myorg", Repo: "myrepo", Labels: []string{"preview"}},
		Values: map[string]string{"host": "pr-{{ .number }}.example.com"},
	}, true)

	if len(params) != 1 {
		t.Fatalf("Expected only the labelled pull request of this repository, got %v", numbers(params))
	}

	got := params[0]
	for key, want := range map[string]any{
		"number":             "1",
		"title":              "First",
		"branch":             "feature/one_two",
		"branch_slug":        "feature-one-two",
		"target_branch":      "main",
		"target_branch_slug": "main",
		"head_sha":           "0123456789abcdef",
		"head_short_sha":     "01234567",
		"head_short_sha_7":   "0123456",
		"author":             "alice",
	} {
		if got[key] != want {
			t.Errorf("Expected %s=%v, got %v", key, want, got[key])
		}
	}
	if labels, _ := got["labels"].([]string); len(labels) != 1 || labels[0] != "preview" {
		t.Errorf("Expected the labels exposed to Go templates, got %v", got["labels"])
	}
	if values, _ := got["values"].(map[string]string); values["host"] != "pr-1.example.com" {
		t.Errorf("Expected .values.host rendered, got %v", got["values"])
	}
}

func TestPullRequestRepoAndFilters(t *testing.T) {
	path := writePullRequests(t, mockPullRequestsYAML)

	for _, tc := range []struct {
		name string
		gen  *v1alpha1.PullRequestGenerator
		want string
	}{
		{
			name: "pull requests without a repo go to every repository",
			gen:  &v1alpha1.PullRequestGenerator{GitLab: &v1alpha1.PullRequestGeneratorGitLab{Project: "myorg/myrepo"}},
			want: "1,3",
		},
		{
			name: "a repo only goes to its own generator",
			gen:  &v1alpha1.PullRequestGenerator{Gitea: &v1alpha1.PullRequestGeneratorGitea{Owner: "myorg", Repo: "other"}},
			want: "1,2,3",
		},
		{
			name: "filters are applied as upstream",
			gen: &v1alpha1.PullRequestGenerator{
				Bitbucket: &v1alpha1.PullRequestGeneratorBitbucket{Owner: "myorg", Repo: "myrepo"},
				Filters:   []v1alpha1.PullRequestGeneratorFilter{{TargetBranchMatch: new("^release$")}},
			},
			want: "3",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := strings.Join(numbers(pullRequestParams(t, path, tc.gen, true)), ","); got != tc.want {
				t.Errorf("Expected pull requests %s, got %s", tc.want, got)
			}
		})
	}
}

// Without goTemplate, labels are not exposed and values are flattened to
// values.<key>, as the upstream generator does.
func TestPullRequestParamsWithoutGoTemplate(t *testing.T) {
	path := writePullRequests(t, mockPullRequestsYAML)
	params := pullRequestParams(t, path, &v1alpha1.PullRequestGenerator{
		Github: &v1alpha1.PullRequestGeneratorGithub{Owner: "myorg", Repo: "myrepo", Labels: []string{"preview"}},
		Values: map[string]string{"host": "pr-{{number}}.example.com"},
	}, false)

	if len(params) != 1 {
		t.Fatalf("Expected one pull request, got %v", numbers(params))
	}
	if _, ok := params[0]["labels"]; ok {
		t.Error("Expected no labels without goTemplate")
	}
	if got := params[0]["values.host"]; got != "pr-1.example.com" {
		t.Errorf("Expected values.host rendered, got %v", got)
	}
}

// Without a file there are no open pull requests, and the render says so rather
// than failing, so that one preview ApplicationSet does not stop a --dir render.
func TestPullRequestsWithoutAFile(t *testing.T) {
	path := writePullRequests(t, `apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: previews
spec:
  goTemplate: true
  generators:
    - pullRequest:
        github:
          owner: myorg
          repo: myrepo
  template:
    metadata:
      name: preview-{{ .number }}
    spec:
      project: default
      source:
        repoURL: https://github.com/myorg/myrepo
        path: examples/appset-pull-request/input
      destination:
        server: https://kubernetes.default.svc
        namespace: preview
`)
	appSet, err := parseApplicationSet(mustRead(t, path))
	if err != nil {
		t.Fatal(err)
	}

	apps, warnings, err := GenerateApplications(appSet, TemplateOptions{RepoRoot: "."})
	if err != nil {
		t.Fatalf("GenerateApplications failed: %v", err)
	}
	if len(apps) != 0 {
		t.Errorf("Expected no Applications, got %d", len(apps))
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "PullRequestsFile") {
		t.Errorf("Expected a warning about the missing pull requests, got %v", warnings)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return data
}

func TestPullRequestsRejectInvalidEntries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		data     string
		contains string
	}{
		{
			name:     "a misspelt field",
			data:     "- number: 1\n  branch: a\n  target_branch: main\n",
			contains: "target_branch",
		},
		{
			name:     "no number",
			data:     "- branch: a\n  title: Untitled\n",
			contains: "needs a positive number",
		},
		{
			name:     "no branch",
			data:     "- number: 4\n",
			contains: "#4 needs a branch",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parsePullRequests([]byte(tc.data))
			if err == nil || !strings.Contains(err.Error(), tc.contains) {
				t.Errorf("Expected an error mentioning %q, got %v", tc.contains, err)
			}
		})
	}
}

func TestPullRequestsWithoutProvider(t *testing.T) {
	path := writePullRequests(t, mockPullRequestsYAML)
	_, err := newMockPullRequestGenerator(path, nil).GenerateParams(
		&v1alpha1.ApplicationSetGenerator{PullRequest: &v1alpha1.PullRequestGenerator{}}, &v1alpha1.ApplicationSet{}, nil)
	if err == nil || !strings.Contains(err.Error(), "no Pull Request provider") {
		t.Errorf("Expected the upstream missing-provider error, got %v", err)
	}
}
