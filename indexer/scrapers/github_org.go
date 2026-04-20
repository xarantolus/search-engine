package scrapers

import (
	"context"
	"fmt"
	"indexer/scrapers/extractors"
	"log"
	"os"
	"shared/config"
	"shared/doc"
	"shared/embedding"
	"strings"

	"github.com/dustin/go-humanize"
	"github.com/google/go-github/v72/github"
	"github.com/meilisearch/meilisearch-go"
)

// ScrapeGithubOrgJob indexes issues and pull requests for all repositories in
// a GitHub organisation or user account.
type ScrapeGithubOrgJob struct {
	Name string
	Org  config.GithubOrg

	Index    meilisearch.IndexManager
	Embedder *embedding.EmbedConfig
	Config   *config.Config

	ghClient *github.Client

	IntervalHelper
}

func (s *ScrapeGithubOrgJob) DisplayName() string {
	return fmt.Sprintf("GitHub Org:%s (%s)", s.Name, s.Org.Owner)
}

func (s *ScrapeGithubOrgJob) Setup() error {
	s.ghClient = GithubClientFromOwner(s.Config, "github.com")
	return nil
}

func (s *ScrapeGithubOrgJob) Run() (err error) {
	logger := log.New(os.Stdout, fmt.Sprintf("[ScrapeGithubOrgJob:%s] ", s.Name), log.LstdFlags)
	logger.Printf("Scraping issues/PRs of GitHub org/user %s\n", s.Org.Owner)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	expirationChecker := NewExpirationChecker(ctx, s.Embedder, logger, s.Index, s.Config.DeleteAfterTime, false)

	repos, err := listGithubOwnerRepos(ctx, s.ghClient, s.Org.Owner, s.Org.IsUser)
	if err != nil {
		return fmt.Errorf("failed to list repos for %s: %w", s.Org.Owner, err)
	}

	batcher := doc.NewDocumentBatcher(s.Index, s.Embedder, s.Config.MaxDocumentSize, doc.MEILISEARCH_SAFE_MAX_COMMIT_SIZE, s.Config.MinCommit, logger)

	for _, repo := range repos {
		repoPath := repo.GetFullName()

		if len(s.Org.IndexReposExcludeGlob) > 0 && AnyGlobMatches(s.Org.IndexReposExcludeGlob, repoPath) {
			logger.Printf("Skipping %q due to index_repos_exclude match", repoPath)
			continue
		}
		if len(s.Org.IndexReposIncludeGlob) > 0 && !AnyGlobMatches(s.Org.IndexReposIncludeGlob, repoPath) {
			logger.Printf("Skipping %q as it is not in index_repos_include match", repoPath)
			continue
		}

		permTag := Cascade(s.Org.IndexReposPermissionTagOverride, s.Org.PermissionTag)

		if s.Org.ScrapeIssues {
			if err := scrapeGithubRepoIssues(ctx, logger, s.ghClient, expirationChecker, batcher, repo, permTag); err != nil {
				logger.Printf("Failed to scrape issues for %s: %v\n", repoPath, err)
			}
		}
		if s.Org.ScrapePullRequests {
			if err := scrapeGithubRepoPRs(ctx, logger, s.ghClient, expirationChecker, batcher, repo, permTag); err != nil {
				logger.Printf("Failed to scrape pull requests for %s: %v\n", repoPath, err)
			}
		}
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

func scrapeGithubRepoIssues(ctx context.Context, logger *log.Logger, client *github.Client, expirationChecker *ExpirationChecker, batcher *doc.DocumentBatcher, repo *github.Repository, permTag string) error {
	owner := repo.GetOwner().GetLogin()
	repoName := repo.GetName()

	opts := &github.IssueListByRepoOptions{
		State:       "all",
		ListOptions: github.ListOptions{PerPage: perPageEntries},
	}
	for {
		issues, resp, err := client.Issues.ListByRepo(ctx, owner, repoName, opts)
		if err != nil {
			return err
		}
		for _, issue := range issues {
			if issue.IsPullRequest() {
				continue
			}

			lastMod := issue.GetUpdatedAt().Unix()
			if lastMod == 0 {
				lastMod = issue.GetCreatedAt().Unix()
			}

			docID := hashURL(issue.GetHTMLURL())
			exist, reason, err := expirationChecker.IsDocUpToDate(docID, lastMod, "github_meta_v1", permTag)
			if err == nil && exist {
				continue
			}

			text, err := extractGithubIssueText(ctx, client, owner, repoName, issue)
			if err != nil {
				logger.Printf("Failed to extract text from issue #%d in %s/%s: %v\n", issue.GetNumber(), owner, repoName, err)
			}

			slug := fmt.Sprintf("%s/%s/issues/%d", owner, repoName, issue.GetNumber())
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
				PermissionTag: permTag,
			}

			numCommitted, err := batcher.AddDocument(document)
			if err != nil {
				logger.Printf("Failed to index issue %s: %v\n", slug, err)
				continue
			}
			if numCommitted > 0 {
				logger.Printf("Committed %d items\n", numCommitted)
			}
			logger.Printf("Staged %s (%s, %s)\n", slug, humanize.Bytes(uint64(len(document.Content))), reason)
		}
		if resp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage
	}
	return nil
}

func scrapeGithubRepoPRs(ctx context.Context, logger *log.Logger, client *github.Client, expirationChecker *ExpirationChecker, batcher *doc.DocumentBatcher, repo *github.Repository, permTag string) error {
	owner := repo.GetOwner().GetLogin()
	repoName := repo.GetName()

	opts := &github.PullRequestListOptions{
		State:       "all",
		ListOptions: github.ListOptions{PerPage: perPageEntries},
	}
	for {
		prs, resp, err := client.PullRequests.List(ctx, owner, repoName, opts)
		if err != nil {
			return err
		}
		for _, pr := range prs {
			lastMod := pr.GetUpdatedAt().Unix()
			if lastMod == 0 {
				lastMod = pr.GetCreatedAt().Unix()
			}

			docID := hashURL(pr.GetHTMLURL())
			exist, reason, err := expirationChecker.IsDocUpToDate(docID, lastMod, "github_meta_v1", permTag)
			if err == nil && exist {
				continue
			}

			text, err := extractGithubPRText(ctx, client, owner, repoName, pr)
			if err != nil {
				logger.Printf("Failed to extract text from PR #%d in %s/%s: %v\n", pr.GetNumber(), owner, repoName, err)
			}

			slug := fmt.Sprintf("%s/%s/pull/%d", owner, repoName, pr.GetNumber())
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
				PermissionTag: permTag,
			}

			numCommitted, err := batcher.AddDocument(document)
			if err != nil {
				logger.Printf("Failed to index PR %s: %v\n", slug, err)
				continue
			}
			if numCommitted > 0 {
				logger.Printf("Committed %d items\n", numCommitted)
			}
			logger.Printf("Staged %s (%s, %s)\n", slug, humanize.Bytes(uint64(len(document.Content))), reason)
		}
		if resp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage
	}
	return nil
}

// listGithubOwnerRepos lists all repos for a GitHub organisation or user.
func listGithubOwnerRepos(ctx context.Context, client *github.Client, owner string, isUser bool) ([]*github.Repository, error) {
	var repos []*github.Repository
	if isUser {
		// List(ctx, "") returns all repos for the authenticated user including
		// private ones. Filter to the requested owner afterwards so that repos
		// from orgs or other collaborations are excluded.
		opts := &github.RepositoryListByAuthenticatedUserOptions{
			ListOptions: github.ListOptions{PerPage: perPageEntries},
		}
		for {
			batch, resp, err := client.Repositories.ListByAuthenticatedUser(ctx, opts)
			if err != nil {
				return repos, err
			}
			for _, r := range batch {
				if strings.EqualFold(r.GetOwner().GetLogin(), owner) {
					repos = append(repos, r)
				}
			}
			if resp.NextPage == 0 {
				break
			}
			opts.ListOptions.Page = resp.NextPage
		}
	} else {
		opts := &github.RepositoryListByOrgOptions{
			ListOptions: github.ListOptions{PerPage: perPageEntries},
		}
		for {
			batch, resp, err := client.Repositories.ListByOrg(ctx, owner, opts)
			if err != nil {
				return repos, err
			}
			repos = append(repos, batch...)
			if resp.NextPage == 0 {
				break
			}
			opts.ListOptions.Page = resp.NextPage
		}
	}
	return repos, nil
}

// ListGithubOwnerRepos is an exported wrapper for use from main.go.
func ListGithubOwnerRepos(ctx context.Context, client *github.Client, owner string, isUser bool) ([]*github.Repository, error) {
	return listGithubOwnerRepos(ctx, client, owner, isUser)
}
