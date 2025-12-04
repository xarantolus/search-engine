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
	"strconv"
	"strings"

	"github.com/dustin/go-humanize"
	"github.com/meilisearch/meilisearch-go"
	gitlab "gitlab.com/gitlab-org/api/client-go"
)

type ScrapeGitLabGroupJob struct {
	Name string

	Group config.GitlabGroup

	Index  meilisearch.IndexManager
	Config *config.Config

	Embedder *embedding.EmbedConfig

	gitLabClient *gitlab.Client

	group *gitlab.Group

	IntervalHelper
}

func (s *ScrapeGitLabGroupJob) DisplayName() string {
	return fmt.Sprintf("GitLab Group:%s (%s)", s.group.Name, s.Name)
}

func (s *ScrapeGitLabGroupJob) Setup() (err error) {
	s.gitLabClient, err = GitLabClientFromURL(s.Config, s.Group.Host)
	if err != nil {
		return fmt.Errorf("failed to create GitLab client: %w", err)
	}

	s.group, _, err = s.gitLabClient.Groups.GetGroup(s.Group.ID, nil)

	return
}

func (s *ScrapeGitLabGroupJob) Run() (err error) {
	logger := log.New(os.Stdout, fmt.Sprintf("[ScrapeGitLabGroupJob:%s] ", s.Name), log.LstdFlags)

	logger.Printf("Scraping issues/PRs of group %s of %s\n", s.group.Path, s.Group.Host)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var expirationChecker = NewExpirationChecker(ctx, s.Embedder, logger, s.Index, s.Config.DeleteAfterTime, false)

	var (
		outputChannel = make(chan any, perPageEntries)
		errorChannel  = make(chan error)
	)
	go func() {
		var issueErr, prErr error
		if s.Group.ScrapeIssues {
			issueErr = listAllGroupIssues(s.gitLabClient, s.Group.ID, outputChannel)
		}
		if s.Group.ScrapeMergeRequests {
			prErr = listAllGroupMergeRequests(s.gitLabClient, s.Group.ID, outputChannel)
		}

		close(outputChannel)
		if issueErr != nil && prErr != nil {
			errorChannel <- fmt.Errorf("failed to list issues and PRs: %w, %w", issueErr, prErr)
		} else if issueErr != nil {
			errorChannel <- fmt.Errorf("failed to list issues: %w", issueErr)
		} else if prErr != nil {
			errorChannel <- fmt.Errorf("failed to list PRs: %w", prErr)
		} else {
			errorChannel <- nil
		}
	}()

	// For every issue, create a long string that just contains all the text from the issue.
	var documentBatcher = doc.NewDocumentBatcher(s.Index, s.Embedder, s.Config.MaxDocumentSize, doc.MEILISEARCH_SAFE_MAX_COMMIT_SIZE, s.Config.MinCommit, logger)

	for issue := range outputChannel {
		var (
			docID       string
			lastMod     int64
			slug        string
			iid         int
			title       string
			webURL      string
			projectPath string
		)
		switch issue := issue.(type) {
		case *gitlab.Issue:
			docID = hashURL(issue.WebURL)
			if issue.UpdatedAt != nil {
				lastMod = issue.UpdatedAt.Unix()
			} else if issue.CreatedAt != nil {
				lastMod = issue.CreatedAt.Unix()
			}
			slug = strings.Split(issue.References.Full, "#")[0] + "/issues/" + strconv.Itoa(issue.IID)
			projectPath = cutEnd(issue.References.Full, issue.References.Short)
			iid = issue.IID
			title = issue.Title
			webURL = issue.WebURL
		case *gitlab.BasicMergeRequest:
			docID = hashURL(issue.WebURL)
			if issue.UpdatedAt != nil {
				lastMod = issue.UpdatedAt.Unix()
			} else if issue.CreatedAt != nil {
				lastMod = issue.CreatedAt.Unix()
			}
			slug = strings.Split(issue.References.Full, "!")[0] + "/merge_requests/" + strconv.Itoa(issue.IID)
			projectPath = cutEnd(issue.References.Full, issue.References.Short)
			iid = issue.IID
			title = issue.Title
			webURL = issue.WebURL
		default:
			panic("invalid type: " + fmt.Sprintf("%T", issue))
		}

		if len(s.Group.IndexReposExcludeGlob) > 0 && AnyGlobMatches(s.Group.IndexReposExcludeGlob, projectPath) {
			logger.Printf("Skipping %q due to index_repos_exclude match", projectPath)
			continue
		}

		if len(s.Group.IndexReposIncludeGlob) > 0 && !AnyGlobMatches(s.Group.IndexReposIncludeGlob, projectPath) {
			logger.Printf("Skipping %q as it is not included in index_repos_include match", projectPath)
			continue
		}

		exist, reason, err := expirationChecker.IsDocUpToDate(docID, lastMod, "gitlab_meta_v4", s.Group.PermissionTag)
		if err == nil && exist {
			continue
		}

		var text string
		switch issue := issue.(type) {
		case *gitlab.Issue:
			text, err = extractAllTextFromIssue(s.gitLabClient, issue)
		case *gitlab.BasicMergeRequest:
			text, err = extractAllTextFromMergeRequest(s.gitLabClient, issue)
		}
		if err != nil {
			logger.Printf("Failed to extract text from %s %d: %v\n", slug, iid, err)
		}
		var document = doc.Document{
			ID:            docID,
			URL:           webURL,
			Content:       text,
			Slug:          slug,
			Title:         title,
			LastModified:  doc.FloatyNumber(lastMod),
			ReIndex:       extractors.ShouldReindex(err),
			Version:       doc.DocumentVersion,
			IsCode:        false,
			Extractor:     "gitlab_meta_v4",
			PermissionTag: s.Group.PermissionTag,
		}
		numCommitted, err := documentBatcher.AddDocument(document)
		if err != nil {
			logger.Printf("Failed to index issue %s %d: %v\n", slug, iid, err)
			continue
		}
		if numCommitted > 0 {
			logger.Printf("Committed %d issues\n", numCommitted)
		}
		logger.Printf("Staged %s (%s, %s)\n", slug, humanize.Bytes(uint64(len(document.Content))), reason)
	}

	if err := <-errorChannel; err != nil {
		logger.Printf("Failed to list issues/PRs: %v\n", err)
	}
	close(errorChannel)

	numCommitted, err := documentBatcher.Commit()
	if err != nil {
		return fmt.Errorf("%s: failed to commit issues/PRs: %w", s.Name, err)
	}
	if numCommitted > 0 {
		logger.Printf("Committed %d issues/PRs", numCommitted)
	}

	return nil
}

func listAllGroupIssues(client *gitlab.Client, groupID string, outputChannel chan any) error {
	options := &gitlab.ListGroupIssuesOptions{
		ListOptions: gitlab.ListOptions{
			Page:    1,
			PerPage: perPageEntries,
		},
	}

	for {
		issues, resp, err := client.Issues.ListGroupIssues(groupID, options)
		if err != nil {
			return err
		}

		for _, issue := range issues {
			if issue.Confidential {
				continue
			}
			outputChannel <- issue
		}

		if resp.CurrentPage >= resp.TotalPages {
			break
		}

		options.Page = resp.NextPage
	}

	return nil
}

func listAllGroupMergeRequests(client *gitlab.Client, groupID string, outputChannel chan any) error {
	options := &gitlab.ListGroupMergeRequestsOptions{
		ListOptions: gitlab.ListOptions{
			Page:    1,
			PerPage: perPageEntries,
		},
	}

	for {
		mergeRequests, resp, err := client.MergeRequests.ListGroupMergeRequests(groupID, options)
		if err != nil {
			return err
		}

		for _, mergeRequest := range mergeRequests {
			outputChannel <- mergeRequest
		}

		if resp.CurrentPage >= resp.TotalPages {
			break
		}

		options.Page = resp.NextPage
	}

	return nil
}

func cutEnd(s string, end string) string {
	if strings.HasSuffix(s, end) {
		return s[:len(s)-len(end)]
	}
	return s
}
