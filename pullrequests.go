package renderer

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/gosimple/slug"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/argoproj/argo-cd/v3/applicationset/generators"
	pullrequest "github.com/argoproj/argo-cd/v3/applicationset/services/pull_request"
	"github.com/argoproj/argo-cd/v3/applicationset/utils"
	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
)

// mockPullRequest is one open pull request as written in a --pull-requests file.
// The fields are the ones every SCM provider reports to the generator.
type mockPullRequest struct {
	// Repo restricts the pull request to the generators pointing at this
	// repository, written the way the generator names it — see pullRequestRepo.
	// Left empty, the pull request is reported for every repository.
	Repo         string   `json:"repo,omitempty"`
	Number       int64    `json:"number"`
	Title        string   `json:"title,omitempty"`
	Branch       string   `json:"branch"`
	TargetBranch string   `json:"targetBranch,omitempty"`
	HeadSHA      string   `json:"headSHA,omitempty"`
	Labels       []string `json:"labels,omitempty"`
	Author       string   `json:"author,omitempty"`
}

// loadPullRequests reads the mocked pull requests from a file or from every
// *.yaml/*.yml file in a directory. Each document is a list of pull requests, so
// that a file can hold all of them without separators.
func loadPullRequests(path string) ([]mockPullRequest, error) {
	files, err := yamlFiles(path, "pull requests")
	if err != nil {
		return nil, err
	}

	var pulls []mockPullRequest
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("failed to read pull requests from %q: %w", file, err)
		}

		filePulls, err := parsePullRequests(data)
		if err != nil {
			return nil, fmt.Errorf("failed to parse pull requests from %q: %w", file, err)
		}

		pulls = append(pulls, filePulls...)
	}

	return pulls, nil
}

func parsePullRequests(data []byte) ([]mockPullRequest, error) {
	var pulls []mockPullRequest

	reader := k8syaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	for {
		doc, err := reader.Read()
		if err == io.EOF {
			return pulls, nil
		}
		if err != nil {
			return nil, err
		}

		// Strict, so that a misspelt field such as target_branch is reported
		// instead of quietly coming out empty in every generated Application.
		var docPulls []mockPullRequest
		if err := yaml.UnmarshalStrict(doc, &docPulls); err != nil {
			return nil, err
		}

		for _, pull := range docPulls {
			if pull.Number <= 0 {
				return nil, fmt.Errorf("pull request %q needs a positive number", pull.Title)
			}
			if pull.Branch == "" {
				return nil, fmt.Errorf("pull request #%d needs a branch", pull.Number)
			}
		}

		pulls = append(pulls, docPulls...)
	}
}

// pullRequestRepo names the repository a pull request generator points at, in the
// form a mocked pull request's repo field is matched against.
func pullRequestRepo(gen *v1alpha1.PullRequestGenerator) (string, error) {
	switch {
	case gen.Github != nil:
		return gen.Github.Owner + "/" + gen.Github.Repo, nil
	case gen.GitLab != nil:
		return gen.GitLab.Project, nil
	case gen.Gitea != nil:
		return gen.Gitea.Owner + "/" + gen.Gitea.Repo, nil
	case gen.BitbucketServer != nil:
		return gen.BitbucketServer.Project + "/" + gen.BitbucketServer.Repo, nil
	case gen.Bitbucket != nil:
		return gen.Bitbucket.Owner + "/" + gen.Bitbucket.Repo, nil
	case gen.AzureDevOps != nil:
		return gen.AzureDevOps.Organization + "/" + gen.AzureDevOps.Project + "/" + gen.AzureDevOps.Repo, nil
	}

	return "", errors.New("no Pull Request provider implementation configured")
}

// pullRequestLabels returns the labels the provider itself filters on: the GitHub,
// GitLab, Gitea and Azure DevOps services only report pull requests carrying every
// one of them.
func pullRequestLabels(gen *v1alpha1.PullRequestGenerator) []string {
	switch {
	case gen.Github != nil:
		return gen.Github.Labels
	case gen.GitLab != nil:
		return gen.GitLab.Labels
	case gen.Gitea != nil:
		return gen.Gitea.Labels
	case gen.AzureDevOps != nil:
		return gen.AzureDevOps.Labels
	}

	return nil
}

// mockPullRequestService stands in for an SCM provider, reporting the mocked pull
// requests of one repository.
type mockPullRequestService struct {
	pulls []*pullrequest.PullRequest
}

func (s *mockPullRequestService) List(_ context.Context) ([]*pullrequest.PullRequest, error) {
	return s.pulls, nil
}

func newMockPullRequestService(gen *v1alpha1.PullRequestGenerator, mocks []mockPullRequest) (pullrequest.PullRequestService, error) {
	repo, err := pullRequestRepo(gen)
	if err != nil {
		return nil, err
	}
	labels := pullRequestLabels(gen)

	var pulls []*pullrequest.PullRequest
	for _, mock := range mocks {
		if mock.Repo != "" && mock.Repo != repo {
			continue
		}
		if !containsAll(mock.Labels, labels) {
			continue
		}

		pulls = append(pulls, &pullrequest.PullRequest{
			Number:       mock.Number,
			Title:        mock.Title,
			Branch:       mock.Branch,
			TargetBranch: mock.TargetBranch,
			HeadSHA:      mock.HeadSHA,
			Labels:       mock.Labels,
			Author:       mock.Author,
		})
	}

	return &mockPullRequestService{pulls: pulls}, nil
}

func containsAll(have, want []string) bool {
	for _, label := range want {
		if !slices.Contains(have, label) {
			return false
		}
	}

	return true
}

// mockPullRequestGenerator is the upstream PullRequestGenerator with the SCM
// provider swapped for the pull requests read from a local file. The upstream
// generator picks its provider through an unexported hook, so the parameter
// building in GenerateParams is a copy of generators.PullRequestGenerator's and
// has to be kept in step with it.
type mockPullRequestGenerator struct {
	pullRequestsFile string
	pulls            []mockPullRequest
	loaded           bool
	// used records that the generator was reached, so that a render without
	// pull requests can be reported.
	used *atomic.Bool
}

func newMockPullRequestGenerator(pullRequestsFile string, used *atomic.Bool) generators.Generator {
	return &mockPullRequestGenerator{pullRequestsFile: pullRequestsFile, used: used}
}

func (g *mockPullRequestGenerator) GetRequeueAfter(_ *v1alpha1.ApplicationSetGenerator) time.Duration {
	return generators.NoRequeueAfter
}

func (g *mockPullRequestGenerator) GetTemplate(appSetGenerator *v1alpha1.ApplicationSetGenerator) *v1alpha1.ApplicationSetTemplate {
	return &appSetGenerator.PullRequest.Template
}

// mockedPullRequests reads the pull requests file once. Without one there are no
// open pull requests, the same as a repository nobody has opened one against, so
// that rendering a whole tree is not stopped by a single preview ApplicationSet.
func (g *mockPullRequestGenerator) mockedPullRequests() ([]mockPullRequest, error) {
	if !g.loaded && g.pullRequestsFile != "" {
		pulls, err := loadPullRequests(g.pullRequestsFile)
		if err != nil {
			return nil, err
		}
		g.pulls, g.loaded = pulls, true
	}

	return g.pulls, nil
}

func (g *mockPullRequestGenerator) GenerateParams(appSetGenerator *v1alpha1.ApplicationSetGenerator, appSet *v1alpha1.ApplicationSet, _ client.Client) ([]map[string]any, error) {
	if appSetGenerator == nil || appSetGenerator.PullRequest == nil {
		return nil, generators.ErrEmptyAppSetGenerator
	}

	if g.used != nil {
		g.used.Store(true)
	}

	mocks, err := g.mockedPullRequests()
	if err != nil {
		return nil, err
	}

	svc, err := newMockPullRequestService(appSetGenerator.PullRequest, mocks)
	if err != nil {
		return nil, fmt.Errorf("failed to select pull request service provider: %w", err)
	}

	pulls, err := pullrequest.ListPullRequests(context.Background(), svc, appSetGenerator.PullRequest.Filters)
	if err != nil {
		return nil, fmt.Errorf("error listing repos: %w", err)
	}

	// The same slug settings the upstream generator applies, so that branch_slug
	// leaves room for a suffix within a DNS label.
	slug.MaxLength = 50
	slug.CustomSub = map[string]string{"_": "-"}

	render := &utils.Render{}
	params := make([]map[string]any, 0, len(pulls))
	for _, pull := range pulls {
		paramMap := map[string]any{
			"number":             strconv.FormatInt(pull.Number, 10),
			"title":              pull.Title,
			"branch":             pull.Branch,
			"branch_slug":        slug.Make(pull.Branch),
			"target_branch":      pull.TargetBranch,
			"target_branch_slug": slug.Make(pull.TargetBranch),
			"head_sha":           pull.HeadSHA,
			"head_short_sha":     pull.HeadSHA[:min(len(pull.HeadSHA), 8)],
			"head_short_sha_7":   pull.HeadSHA[:min(len(pull.HeadSHA), 7)],
			"author":             pull.Author,
		}

		// Labels are only exposed to Go templates, as upstream.
		if appSet.Spec.GoTemplate {
			paramMap["labels"] = pull.Labels
		}

		if err := appendValues(render, appSetGenerator.PullRequest.Values, paramMap, appSet.Spec.GoTemplate, appSet.Spec.GoTemplateOptions); err != nil {
			return nil, fmt.Errorf("failed to append templated values: %w", err)
		}

		params = append(params, paramMap)
	}

	return params, nil
}

// appendValues mirrors the generators package's unexported appendTemplatedValues:
// every value is rendered against the parameters before any of them is added, and
// they land under .values with Go templates or as values.<key> without.
func appendValues(render *utils.Render, values map[string]string, params map[string]any, useGoTemplate bool, goTemplateOptions []string) error {
	rendered := make(map[string]string, len(values))
	for key, value := range values {
		result, err := render.Replace(value, params, useGoTemplate, goTemplateOptions)
		if err != nil {
			return fmt.Errorf("failed to replace templated string: %w", err)
		}
		rendered[key] = result
	}

	if len(rendered) == 0 {
		return nil
	}

	if useGoTemplate {
		params["values"] = rendered
		return nil
	}

	for key, value := range rendered {
		params["values."+key] = value
	}

	return nil
}
