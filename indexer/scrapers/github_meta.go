package scrapers

import (
	"context"
	"fmt"
	"indexer/scrapers/extractors"
	"log"
	"net/url"
	"os"
	"shared/config"
	"shared/doc"
	"shared/embedding"
	"strings"

	"github.com/dustin/go-humanize"
	"github.com/google/go-github/v72/github"
	"github.com/meilisearch/meilisearch-go"
	"golang.org/x/oauth2"
)

// ScrapeGithubMetaJob indexes issues and pull requests for a single GitHub repository.
type ScrapeGithubMetaJob struct {
	Name string
	Repo config.Repository

	Index    meilisearch.IndexManager
	Embedder *embedding.EmbedConfig
	Config   *config.Config

	ghClient *github.Client
	owner    string
	repoName string

	IntervalHelper
}

func (s *ScrapeGithubMetaJob) DisplayName() string {
	return fmt.Sprintf("GitHub Meta:%s (%s)", s.Name, s.Repo.URL)
}

func (s *ScrapeGithubMetaJob) Setup() (err error) {
	s.ghClient, s.owner, s.repoName, err = githubClientFromRepoURL(s.Config, s.Repo.URL)
	return
}

func (s *ScrapeGithubMetaJob) Run() (err error) {
	logger := log.New(os.Stdout, fmt.Sprintf("[ScrapeGithubMetaJob:%s] ", s.Name), log.LstdFlags)
	logger.Printf("Scraping issues/PRs of %s\n", s.Repo.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	expirationChecker := NewExpirationChecker(ctx, s.Embedder, logger, s.Index, s.Config.DeleteAfterTime, false)

	batcher := doc.NewDocumentBatcher(s.Index, s.Embedder, s.Config.MaxDocumentSize, doc.MEILISEARCH_SAFE_MAX_COMMIT_SIZE, s.Config.MinCommit, logger)

	if err := s.scrapeIssues(ctx, logger, expirationChecker, batcher); err != nil {
		logger.Printf("Failed to scrape issues: %v\n", err)
	}
	if err := s.scrapePullRequests(ctx, logger, expirationChecker, batcher); err != nil {
		logger.Printf("Failed to scrape pull requests: %v\n", err)
	}

	numCommitted, err := batcher.Commit()
	if err != nil {
		return fmt.Errorf("%s: failed to commit: %w", s.Name, err)
	}
	if numCommitted > 0 {
		logger.Printf("Committed %d items\n", numCommitted)
	}
	return nil
}

func (s *ScrapeGithubMetaJob) scrapeIssues(ctx context.Context, logger *log.Logger, expirationChecker *ExpirationChecker, batcher *doc.DocumentBatcher) error {
	opts := &github.IssueListByRepoOptions{
		State:       "all",
		ListOptions: github.ListOptions{PerPage: perPageEntries},
	}
	for {
		issues, resp, err := s.ghClient.Issues.ListByRepo(ctx, s.owner, s.repoName, opts)
		if err != nil {
			return err
		}
		for _, issue := range issues {
			// GitHub's ListByRepo returns both issues and PRs; skip PRs here.
			if issue.IsPullRequest() {
				continue
			}
			if err := s.indexIssue(ctx, logger, expirationChecker, batcher, issue); err != nil {
				logger.Printf("Failed to index issue #%d: %v\n", issue.GetNumber(), err)
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage
	}
	return nil
}

func (s *ScrapeGithubMetaJob) scrapePullRequests(ctx context.Context, logger *log.Logger, expirationChecker *ExpirationChecker, batcher *doc.DocumentBatcher) error {
	opts := &github.PullRequestListOptions{
		State:       "all",
		ListOptions: github.ListOptions{PerPage: perPageEntries},
	}
	for {
		prs, resp, err := s.ghClient.PullRequests.List(ctx, s.owner, s.repoName, opts)
		if err != nil {
			return err
		}
		for _, pr := range prs {
			if err := s.indexPR(ctx, logger, expirationChecker, batcher, pr); err != nil {
				logger.Printf("Failed to index PR #%d: %v\n", pr.GetNumber(), err)
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage
	}
	return nil
}

func (s *ScrapeGithubMetaJob) indexIssue(ctx context.Context, logger *log.Logger, expirationChecker *ExpirationChecker, batcher *doc.DocumentBatcher, issue *github.Issue) error {
	lastMod := issue.GetUpdatedAt().Unix()
	if lastMod == 0 {
		lastMod = issue.GetCreatedAt().Unix()
	}

	docID := hashURL(issue.GetHTMLURL())
	exist, reason, err := expirationChecker.IsDocUpToDate(docID, lastMod, "github_meta_v1", s.Repo.PermissionTag)
	if err == nil && exist {
		return nil
	}

	text, err := extractGithubIssueText(ctx, s.ghClient, s.owner, s.repoName, issue)
	if err != nil {
		logger.Printf("Failed to extract text from issue #%d: %v\n", issue.GetNumber(), err)
	}

	slug := fmt.Sprintf("%s/%s/issues/%d", s.owner, s.repoName, issue.GetNumber())
	document := doc.Document{
		ID:            docID,
		URL:           issue.GetHTMLURL(),
		Content:       text,
		Slug:          slug,
		Title:         issue.GetTitle(),
		LastModified:  doc.FloatyNumber(lastMod),
		ReIndex:       extractors.ShouldReindex(err),
		Version:       doc.DocumentVersion,
		IsCode:        false,
		Extractor:     "github_meta_v1",
		PermissionTag: s.Repo.PermissionTag,
	}

	numCommitted, err := batcher.AddDocument(document)
	if err != nil {
		return fmt.Errorf("failed to add document: %w", err)
	}
	if numCommitted > 0 {
		logger.Printf("Committed %d items\n", numCommitted)
	}
	logger.Printf("Staged %s (%s, %s)\n", slug, humanize.Bytes(uint64(len(document.Content))), reason)
	return nil
}

func (s *ScrapeGithubMetaJob) indexPR(ctx context.Context, logger *log.Logger, expirationChecker *ExpirationChecker, batcher *doc.DocumentBatcher, pr *github.PullRequest) error {
	lastMod := pr.GetUpdatedAt().Unix()
	if lastMod == 0 {
		lastMod = pr.GetCreatedAt().Unix()
	}

	docID := hashURL(pr.GetHTMLURL())
	exist, reason, err := expirationChecker.IsDocUpToDate(docID, lastMod, "github_meta_v1", s.Repo.PermissionTag)
	if err == nil && exist {
		return nil
	}

	text, err := extractGithubPRText(ctx, s.ghClient, s.owner, s.repoName, pr)
	if err != nil {
		logger.Printf("Failed to extract text from PR #%d: %v\n", pr.GetNumber(), err)
	}

	slug := fmt.Sprintf("%s/%s/pull/%d", s.owner, s.repoName, pr.GetNumber())
	document := doc.Document{
		ID:            docID,
		URL:           pr.GetHTMLURL(),
		Content:       text,
		Slug:          slug,
		Title:         pr.GetTitle(),
		LastModified:  doc.FloatyNumber(lastMod),
		ReIndex:       extractors.ShouldReindex(err),
		Version:       doc.DocumentVersion,
		IsCode:        false,
		Extractor:     "github_meta_v1",
		PermissionTag: s.Repo.PermissionTag,
	}

	numCommitted, err := batcher.AddDocument(document)
	if err != nil {
		return fmt.Errorf("failed to add document: %w", err)
	}
	if numCommitted > 0 {
		logger.Printf("Committed %d items\n", numCommitted)
	}
	logger.Printf("Staged %s (%s, %s)\n", slug, humanize.Bytes(uint64(len(document.Content))), reason)
	return nil
}

func extractGithubIssueText(ctx context.Context, client *github.Client, owner, repo string, issue *github.Issue) (string, error) {
	var sb strings.Builder
	if issue.User != nil {
		sb.WriteString(issue.User.GetLogin())
	}
	if issue.GetBody() != "" {
		sb.WriteString(": ")
		sb.WriteString(issue.GetBody())
	}

	commentOpts := &github.IssueListCommentsOptions{
		ListOptions: github.ListOptions{PerPage: perPageEntries},
	}
	for {
		comments, resp, err := client.Issues.ListComments(ctx, owner, repo, issue.GetNumber(), commentOpts)
		if err != nil {
			break
		}
		for _, c := range comments {
			sb.WriteString("\n\n")
			sb.WriteString(c.User.GetLogin())
			sb.WriteString(": ")
			sb.WriteString(c.GetBody())
		}
		if resp.NextPage == 0 {
			break
		}
		commentOpts.ListOptions.Page = resp.NextPage
	}
	return sb.String(), nil
}

func extractGithubPRText(ctx context.Context, client *github.Client, owner, repo string, pr *github.PullRequest) (string, error) {
	var sb strings.Builder
	if pr.User != nil {
		sb.WriteString(pr.User.GetLogin())
	}
	if pr.GetBody() != "" {
		sb.WriteString(": ")
		sb.WriteString(pr.GetBody())
	}

	commentOpts := &github.IssueListCommentsOptions{
		ListOptions: github.ListOptions{PerPage: perPageEntries},
	}
	for {
		comments, resp, err := client.Issues.ListComments(ctx, owner, repo, pr.GetNumber(), commentOpts)
		if err != nil {
			break
		}
		for _, c := range comments {
			sb.WriteString("\n\n")
			sb.WriteString(c.User.GetLogin())
			sb.WriteString(": ")
			sb.WriteString(c.GetBody())
		}
		if resp.NextPage == 0 {
			break
		}
		commentOpts.ListOptions.Page = resp.NextPage
	}
	return sb.String(), nil
}

// githubClientFromRepoURL creates a GitHub client and parses owner/repo from the clone URL.
func githubClientFromRepoURL(cfg *config.Config, repoURL string) (client *github.Client, owner, repoName string, err error) {
	u, err := url.Parse(repoURL)
	if err != nil {
		return
	}

	parts := strings.Split(strings.Trim(strings.TrimSuffix(u.Path, ".git"), "/"), "/")
	if len(parts) < 2 {
		err = fmt.Errorf("could not parse owner/repo from URL %s", repoURL)
		return
	}
	owner = parts[len(parts)-2]
	repoName = parts[len(parts)-1]

	client = newGithubClient(cfg, u.Host)
	return
}

// GithubClientFromOwner creates a GitHub client, looking up the token for the given host.
func GithubClientFromOwner(cfg *config.Config, host string) *github.Client {
	return newGithubClient(cfg, host)
}

func newGithubClient(cfg *config.Config, host string) *github.Client {
	var token string
	for _, login := range cfg.GitLogins {
		if login.Host == host {
			token = login.Token
			break
		}
	}

	if token == "" {
		return github.NewClient(nil)
	}

	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	tc := oauth2.NewClient(context.Background(), ts)
	return github.NewClient(tc)
}
