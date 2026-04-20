package scrapers

import (
	"context"
	"fmt"
	"indexer/scrapers/extractors"
	"log"
	"net/http"
	"os"
	"shared/config"
	"shared/doc"
	"shared/embedding"
	"strings"

	"code.gitea.io/sdk/gitea"
	"github.com/dustin/go-humanize"
	"github.com/meilisearch/meilisearch-go"
)

// ScrapeGiteaOrgJob indexes issues and pull requests for all repositories in
// a Gitea organisation (or user account).
type ScrapeGiteaOrgJob struct {
	Name string
	Org  config.GiteaOrg

	Index    meilisearch.IndexManager
	Embedder *embedding.EmbedConfig
	Config   *config.Config

	giteaClient *gitea.Client

	IntervalHelper
}

func (s *ScrapeGiteaOrgJob) DisplayName() string {
	return fmt.Sprintf("Gitea Org:%s (%s/%s)", s.Name, s.Org.Host, s.Org.Owner)
}

func (s *ScrapeGiteaOrgJob) Setup() (err error) {
	s.giteaClient, err = GiteaClientFromURL(s.Config, s.Org.Host)
	return
}

func (s *ScrapeGiteaOrgJob) Run() (err error) {
	logger := log.New(os.Stdout, fmt.Sprintf("[ScrapeGiteaOrgJob:%s] ", s.Name), log.LstdFlags)
	logger.Printf("Scraping issues/PRs of Gitea org/user %s/%s\n", s.Org.Host, s.Org.Owner)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	expirationChecker := NewExpirationChecker(ctx, s.Embedder, logger, s.Index, s.Config.DeleteAfterTime, false)

	repos, err := listGiteaOwnerRepos(s.giteaClient, s.Org.Owner, s.Org.IsUser)
	if err != nil {
		return fmt.Errorf("failed to list repos for %s: %w", s.Org.Owner, err)
	}

	batcher := doc.NewDocumentBatcher(s.Index, s.Embedder, s.Config.MaxDocumentSize, doc.MEILISEARCH_SAFE_MAX_COMMIT_SIZE, s.Config.MinCommit, logger)

	for _, repo := range repos {
		repoPath := repo.Owner.UserName + "/" + repo.Name

		if len(s.Org.IndexReposExcludeGlob) > 0 && AnyGlobMatches(s.Org.IndexReposExcludeGlob, repoPath) {
			logger.Printf("Skipping %q due to index_repos_exclude match", repoPath)
			continue
		}
		if len(s.Org.IndexReposIncludeGlob) > 0 && !AnyGlobMatches(s.Org.IndexReposIncludeGlob, repoPath) {
			logger.Printf("Skipping %q as it is not in index_repos_include match", repoPath)
			continue
		}

		if s.Org.ScrapeIssues {
			if err := s.scrapeRepoIssues(ctx, logger, expirationChecker, batcher, repo); err != nil {
				logger.Printf("Failed to scrape issues for %s: %v\n", repoPath, err)
			}
		}

		if s.Org.ScrapePullRequests {
			if err := s.scrapeRepoPRs(ctx, logger, expirationChecker, batcher, repo); err != nil {
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

func (s *ScrapeGiteaOrgJob) scrapeRepoIssues(
	_ context.Context,
	logger *log.Logger,
	expirationChecker *ExpirationChecker,
	batcher *doc.DocumentBatcher,
	repo *gitea.Repository,
) error {
	owner := repo.Owner.UserName
	repoName := repo.Name
	permTag := Cascade(s.Org.IndexReposPermissionTagOverride, s.Org.PermissionTag)

	issueOpts := gitea.ListIssueOption{
		ListOptions: gitea.ListOptions{Page: 1, PageSize: perPageEntries},
		Type:        gitea.IssueTypeIssue,
		State:       gitea.StateAll,
	}
	for {
		issues, resp, err := s.giteaClient.ListRepoIssues(owner, repoName, issueOpts)
		if err != nil {
			if resp != nil && resp.StatusCode == http.StatusNotFound {
				return nil // issues disabled for this repo
			}
			return err
		}

		for _, issue := range issues {
			var lastMod int64
			if !issue.Updated.IsZero() {
				lastMod = issue.Updated.Unix()
			} else if !issue.Created.IsZero() {
				lastMod = issue.Created.Unix()
			}

			docID := hashURL(issue.HTMLURL)
			exist, reason, err := expirationChecker.IsDocUpToDate(docID, lastMod, "gitea_meta_v1", permTag)
			if err == nil && exist {
				continue
			}

			text, err := extractGiteaIssueText(s.giteaClient, owner, repoName, issue)
			if err != nil {
				logger.Printf("Failed to extract text from issue #%d in %s/%s: %v\n", issue.Index, owner, repoName, err)
			}

			slug := fmt.Sprintf("%s/%s/issues/%d", owner, repoName, issue.Index)
			document := doc.Document{
				ID:            docID,
				URL:           issue.HTMLURL,
				Content:       text,
				Slug:          slug,
				Title:         issue.Title,
				LastModified:  doc.FloatyNumber(lastMod),
				ReIndex:       extractors.ShouldReindex(err),
				Version:       doc.DocumentVersion,
				IsCode:        false,
				Extractor:     "gitea_meta_v1",
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
		issueOpts.Page = resp.NextPage
	}
	return nil
}

func (s *ScrapeGiteaOrgJob) scrapeRepoPRs(
	_ context.Context,
	logger *log.Logger,
	expirationChecker *ExpirationChecker,
	batcher *doc.DocumentBatcher,
	repo *gitea.Repository,
) error {
	owner := repo.Owner.UserName
	repoName := repo.Name
	permTag := Cascade(s.Org.IndexReposPermissionTagOverride, s.Org.PermissionTag)

	prOpts := gitea.ListPullRequestsOptions{
		ListOptions: gitea.ListOptions{Page: 1, PageSize: perPageEntries},
		State:       gitea.StateAll,
	}
	for {
		prs, resp, err := s.giteaClient.ListRepoPullRequests(owner, repoName, prOpts)
		if err != nil {
			if resp != nil && resp.StatusCode == http.StatusNotFound {
				return nil // pull requests disabled for this repo
			}
			return err
		}

		for _, pr := range prs {
			var lastMod int64
			if pr.Updated != nil {
				lastMod = pr.Updated.Unix()
			} else if pr.Created != nil {
				lastMod = pr.Created.Unix()
			}

			docID := hashURL(pr.HTMLURL)
			exist, reason, err := expirationChecker.IsDocUpToDate(docID, lastMod, "gitea_meta_v1", permTag)
			if err == nil && exist {
				continue
			}

			text, err := extractGiteaPRText(s.giteaClient, owner, repoName, pr)
			if err != nil {
				logger.Printf("Failed to extract text from PR #%d in %s/%s: %v\n", pr.Index, owner, repoName, err)
			}

			slug := fmt.Sprintf("%s/%s/pulls/%d", owner, repoName, pr.Index)
			document := doc.Document{
				ID:            docID,
				URL:           pr.HTMLURL,
				Content:       text,
				Slug:          slug,
				Title:         pr.Title,
				LastModified:  doc.FloatyNumber(lastMod),
				ReIndex:       extractors.ShouldReindex(err),
				Version:       doc.DocumentVersion,
				IsCode:        false,
				Extractor:     "gitea_meta_v1",
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
		prOpts.Page = resp.NextPage
	}
	return nil
}

func extractGiteaIssueText(client *gitea.Client, owner, repo string, issue *gitea.Issue) (string, error) {
	var sb strings.Builder
	sb.WriteString(issue.Poster.FullName)
	if issue.Body != "" {
		sb.WriteString(": ")
		sb.WriteString(issue.Body)
	}

	issueCommentOpts := gitea.ListIssueCommentOptions{
		ListOptions: gitea.ListOptions{Page: 1, PageSize: perPageEntries},
	}
	for {
		comments, resp, err := client.ListIssueComments(owner, repo, issue.Index, issueCommentOpts)
		if err != nil {
			break
		}
		for _, c := range comments {
			sb.WriteString("\n\n")
			sb.WriteString(c.Poster.FullName)
			sb.WriteString(": ")
			sb.WriteString(c.Body)
		}
		if resp.NextPage == 0 {
			break
		}
		issueCommentOpts.Page = resp.NextPage
	}
	return sb.String(), nil
}

func extractGiteaPRText(client *gitea.Client, owner, repo string, pr *gitea.PullRequest) (string, error) {
	var sb strings.Builder
	sb.WriteString(pr.Poster.FullName)
	if pr.Body != "" {
		sb.WriteString(": ")
		sb.WriteString(pr.Body)
	}

	prCommentOpts := gitea.ListIssueCommentOptions{
		ListOptions: gitea.ListOptions{Page: 1, PageSize: perPageEntries},
	}
	for {
		comments, resp, err := client.ListIssueComments(owner, repo, pr.Index, prCommentOpts)
		if err != nil {
			break
		}
		for _, c := range comments {
			sb.WriteString("\n\n")
			sb.WriteString(c.Poster.FullName)
			sb.WriteString(": ")
			sb.WriteString(c.Body)
		}
		if resp.NextPage == 0 {
			break
		}
		prCommentOpts.Page = resp.NextPage
	}
	return sb.String(), nil
}

// ListGiteaOwnerRepos is an exported wrapper for use from main.go.
func ListGiteaOwnerRepos(client *gitea.Client, owner string, isUser bool) ([]*gitea.Repository, error) {
	return listGiteaOwnerRepos(client, owner, isUser)
}

// listGiteaOwnerRepos lists all repos for a Gitea organisation or user.
// For user repos it uses the authenticated /user/repos endpoint so that
// private repositories owned by the token holder are included.
func listGiteaOwnerRepos(client *gitea.Client, owner string, isUser bool) ([]*gitea.Repository, error) {
	var repos []*gitea.Repository
	listOpts := gitea.ListOptions{Page: 1, PageSize: perPageEntries}
	for {
		var (
			batch []*gitea.Repository
			resp  *gitea.Response
			err   error
		)
		if isUser {
			// ListMyRepos returns all repos visible to the authenticated user
			// (including private ones), unlike ListUserRepos which is public-only.
			// Filter to repos actually owned by `owner` afterwards.
			batch, resp, err = client.ListMyRepos(gitea.ListReposOptions{
				ListOptions: listOpts,
			})
		} else {
			batch, resp, err = client.ListOrgRepos(owner, gitea.ListOrgReposOptions{
				ListOptions: listOpts,
			})
		}
		if err != nil {
			return repos, err
		}
		for _, r := range batch {
			if !isUser || strings.EqualFold(r.Owner.UserName, owner) {
				repos = append(repos, r)
			}
		}
		if resp.NextPage == 0 {
			break
		}
		listOpts.Page = resp.NextPage
	}
	return repos, nil
}
