package scrapers

import (
	"context"
	"fmt"
	"indexer/scrapers/extractors"
	"log"
	"net/http"
	"net/url"
	"os"
	"shared/config"
	"shared/doc"
	"shared/embedding"
	"strings"

	"code.gitea.io/sdk/gitea"
	"github.com/dustin/go-humanize"
	"github.com/meilisearch/meilisearch-go"
)

// ScrapeGiteaMetaJob indexes issues and pull requests for a single Gitea repository.
type ScrapeGiteaMetaJob struct {
	Name string
	Repo config.Repository

	Index    meilisearch.IndexManager
	Embedder *embedding.EmbedConfig
	Config   *config.Config

	giteaClient *gitea.Client
	owner       string
	repoName    string

	IntervalHelper
}

func (s *ScrapeGiteaMetaJob) DisplayName() string {
	return fmt.Sprintf("Gitea Meta:%s (%s)", s.Name, s.Repo.URL)
}

func (s *ScrapeGiteaMetaJob) Setup() (err error) {
	s.giteaClient, s.owner, s.repoName, err = giteaClientFromRepoURL(s.Config, s.Repo.URL)
	return
}

func (s *ScrapeGiteaMetaJob) Run() (err error) {
	logger := log.New(os.Stdout, fmt.Sprintf("[ScrapeGiteaMetaJob:%s] ", s.Name), log.LstdFlags)
	logger.Printf("Scraping issues/PRs of %s\n", s.Repo.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	expirationChecker := NewExpirationChecker(ctx, s.Embedder, logger, s.Index, s.Config.DeleteAfterTime, false)

	type item struct {
		id      int64
		number  int
		kind    string // "issues" or "pulls"
		title   string
		webURL  string
		lastMod int64
		text    string
	}

	var items []item

	// Fetch issues
	issueListOpts := gitea.ListIssueOption{
		ListOptions: gitea.ListOptions{Page: 1, PageSize: perPageEntries},
		Type:        gitea.IssueTypeIssue,
		State:       gitea.StateAll,
	}
	for {
		issues, resp, err := s.giteaClient.ListRepoIssues(s.owner, s.repoName, issueListOpts)
		if err != nil {
			if resp != nil && resp.StatusCode == http.StatusNotFound {
				break // issues disabled for this repo
			}
			logger.Printf("Failed to list issues (page %d): %v\n", issueListOpts.Page, err)
			break
		}
		for _, issue := range issues {
			var lastMod int64
			if !issue.Updated.IsZero() {
				lastMod = issue.Updated.Unix()
			} else if !issue.Created.IsZero() {
				lastMod = issue.Created.Unix()
			}
			items = append(items, item{
				id:      issue.ID,
				number:  int(issue.Index),
				kind:    "issues",
				title:   issue.Title,
				webURL:  issue.HTMLURL,
				lastMod: lastMod,
			})
		}
		if resp.NextPage == 0 {
			break
		}
		issueListOpts.Page = resp.NextPage
	}

	// Fetch pull requests
	prListOpts := gitea.ListPullRequestsOptions{
		ListOptions: gitea.ListOptions{Page: 1, PageSize: perPageEntries},
		State:       gitea.StateAll,
	}
	for {
		prs, resp, err := s.giteaClient.ListRepoPullRequests(s.owner, s.repoName, prListOpts)
		if err != nil {
			if resp != nil && resp.StatusCode == http.StatusNotFound {
				break // pull requests disabled for this repo
			}
			logger.Printf("Failed to list pull requests (page %d): %v\n", prListOpts.Page, err)
			break
		}
		for _, pr := range prs {
			var lastMod int64
			if pr.Updated != nil {
				lastMod = pr.Updated.Unix()
			} else if pr.Created != nil {
				lastMod = pr.Created.Unix()
			}
			items = append(items, item{
				id:      pr.ID,
				number:  int(pr.Index),
				kind:    "pulls",
				title:   pr.Title,
				webURL:  pr.HTMLURL,
				lastMod: lastMod,
			})
		}
		if resp.NextPage == 0 {
			break
		}
		prListOpts.Page = resp.NextPage
	}

	batcher := doc.NewDocumentBatcher(s.Index, s.Embedder, s.Config.MaxDocumentSize, doc.MEILISEARCH_SAFE_MAX_COMMIT_SIZE, s.Config.MinCommit, logger)

	for _, it := range items {
		docID := hashURL(it.webURL)

		exist, reason, err := expirationChecker.IsDocUpToDate(docID, it.lastMod, "gitea_meta_v1", s.Repo.PermissionTag)
		if err == nil && exist {
			continue
		}

		var text string
		if it.kind == "issues" {
			text, err = s.extractIssueText(int(it.number))
		} else {
			text, err = s.extractPRText(int(it.number))
		}
		if err != nil {
			logger.Printf("Failed to extract text from %s #%d: %v\n", it.kind, it.number, err)
		}

		document := doc.Document{
			ID:            docID,
			URL:           it.webURL,
			Content:       text,
			Slug:          fmt.Sprintf("%s/%s/%s/%d", s.owner, s.repoName, it.kind, it.number),
			Title:         it.title,
			LastModified:  doc.FloatyNumber(it.lastMod),
			ReIndex:       extractors.ShouldReindex(err),
			Version:       doc.DocumentVersion,
			IsCode:        false,
			Extractor:     "gitea_meta_v1",
			PermissionTag: s.Repo.PermissionTag,
		}

		numCommitted, err := batcher.AddDocument(document)
		if err != nil {
			logger.Printf("Failed to index %s #%d: %v\n", it.kind, it.number, err)
			continue
		}
		if numCommitted > 0 {
			logger.Printf("Committed %d items\n", numCommitted)
		}
		logger.Printf("Staged %s/%s/%s/%d (%s, %s)\n", s.owner, s.repoName, it.kind, it.number, humanize.Bytes(uint64(len(document.Content))), reason)
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

func (s *ScrapeGiteaMetaJob) extractIssueText(number int) (string, error) {
	issue, _, err := s.giteaClient.GetIssue(s.owner, s.repoName, int64(number))
	if err != nil {
		return "", err
	}

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
		comments, resp, err := s.giteaClient.ListIssueComments(s.owner, s.repoName, int64(number), issueCommentOpts)
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

func (s *ScrapeGiteaMetaJob) extractPRText(number int) (string, error) {
	pr, _, err := s.giteaClient.GetPullRequest(s.owner, s.repoName, int64(number))
	if err != nil {
		return "", err
	}

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
		comments, resp, err := s.giteaClient.ListIssueComments(s.owner, s.repoName, int64(number), prCommentOpts)
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

// giteaClientFromRepoURL creates a Gitea client for the given repo clone URL,
// and returns the parsed owner and repo name.
func giteaClientFromRepoURL(cfg *config.Config, repoURL string) (client *gitea.Client, owner, repoName string, err error) {
	u, err := url.Parse(repoURL)
	if err != nil {
		return
	}

	var token string
	for _, login := range cfg.GitLogins {
		if login.Host == u.Host {
			token = login.Token
			break
		}
	}

	baseURL := *u
	baseURL.Path = ""

	parts := strings.Split(strings.Trim(strings.TrimSuffix(u.Path, ".git"), "/"), "/")
	if len(parts) < 2 {
		err = fmt.Errorf("could not parse owner/repo from URL %s", repoURL)
		return
	}
	owner = parts[len(parts)-2]
	repoName = parts[len(parts)-1]

	var opts []gitea.ClientOption
	if token != "" {
		opts = append(opts, gitea.SetToken(token))
	}
	client, err = gitea.NewClient(baseURL.String(), opts...)
	return
}

// GiteaClientFromURL creates a Gitea client for the given instance base URL.
func GiteaClientFromURL(cfg *config.Config, instanceURL string) (*gitea.Client, error) {
	u, err := url.Parse(instanceURL)
	if err != nil {
		return nil, err
	}

	var token string
	for _, login := range cfg.GitLogins {
		if login.Host == u.Host {
			token = login.Token
			break
		}
	}
	if token == "" {
		return nil, fmt.Errorf("gitea client: no token found for %s", u.Host)
	}

	baseURL := *u
	baseURL.Path = ""

	return gitea.NewClient(baseURL.String(), gitea.SetToken(token))
}
