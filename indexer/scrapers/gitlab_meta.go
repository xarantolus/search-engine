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
	"github.com/meilisearch/meilisearch-go"
	gitlab "gitlab.com/gitlab-org/api/client-go"
)

type ScrapeGitLabMetaJob struct {
	Name string
	Repo config.Repository

	Index meilisearch.IndexManager

	Embedder *embedding.EmbedConfig
	Config   *config.Config

	gitLabClient *gitlab.Client

	projectPath string

	IntervalHelper
}

func (s *ScrapeGitLabMetaJob) DisplayName() string {
	return fmt.Sprintf("GitLab Meta:%s (%s)", s.Name, s.Repo.URL)
}

func (s *ScrapeGitLabMetaJob) Setup() (err error) {
	urlParsed, err := url.Parse(s.Repo.URL)
	if err != nil {
		return
	}

	s.projectPath = strings.TrimPrefix(strings.TrimSuffix(urlParsed.Path, ".git"), "/")

	s.gitLabClient, err = GitLabClientFromURL(s.Config, s.Repo.URL)
	if err != nil {
		return fmt.Errorf("failed to create GitLab client: %w", err)
	}

	return
}

const perPageEntries = 100

func (s *ScrapeGitLabMetaJob) Run() (err error) {
	logger := log.New(os.Stdout, fmt.Sprintf("[ScrapeGitLabMetaJob:%s] ", s.Name), log.LstdFlags)
	logger.Printf("Scraping issues/PRs of %s\n", s.Repo.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var expirationChecker = NewExpirationChecker(ctx, s.Embedder, logger, s.Index, s.Config.DeleteAfterTime, false)

	var (
		outputChannel = make(chan any, perPageEntries)
		errorChannel  = make(chan error)
	)
	go func() {
		issueErr := listAllProjectIssues(s.gitLabClient, s.projectPath, outputChannel)
		prErr := listAllProjectMergeRequests(s.gitLabClient, s.projectPath, outputChannel)
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
			docID      string
			lastMod    int64
			slugMarker string
			iid        int
			title      string
			webURL     string
		)
		switch issue := issue.(type) {
		case *gitlab.Issue:
			docID = hashURL(issue.WebURL)
			if issue.UpdatedAt != nil {
				lastMod = issue.UpdatedAt.Unix()
			} else if issue.CreatedAt != nil {
				lastMod = issue.CreatedAt.Unix()
			}
			slugMarker = "issues"
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
			slugMarker = "merge_requests"
			iid = issue.IID
			title = issue.Title
			webURL = issue.WebURL
		default:
			panic("invalid type: " + fmt.Sprintf("%T", issue))
		}

		exist, reason, err := expirationChecker.IsDocUpToDate(docID, lastMod, "gitlab_meta_v4", s.Repo.PermissionTag)
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
			logger.Printf("Failed to extract text from %s %d: %v\n", slugMarker, iid, err)
		}
		var document = doc.Document{
			ID:            docID,
			URL:           webURL,
			Content:       text,
			Slug:          fmt.Sprintf("%s/%s/%d", s.projectPath, slugMarker, iid),
			Title:         title,
			LastModified:  doc.FloatyNumber(lastMod),
			ReIndex:       extractors.ShouldReindex(err),
			Version:       doc.DocumentVersion,
			IsCode:        false,
			Extractor:     "gitlab_meta_v4",
			PermissionTag: s.Repo.PermissionTag,
		}
		numCommitted, err := documentBatcher.AddDocument(document)
		if err != nil {
			logger.Printf("Failed to index issue %s %d: %v\n", slugMarker, iid, err)
			continue
		}
		if numCommitted > 0 {
			logger.Printf("Committed %d issues\n", numCommitted)
		}
		logger.Printf("Staged %s/%s/%d (%s, %s)\n", s.projectPath, slugMarker, iid, humanize.Bytes(uint64(len(document.Content))), reason)
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

func extractAllTextFromIssue(client *gitlab.Client, issue_or_pr *gitlab.Issue) (s string, err error) {
	var sb strings.Builder

	sb.WriteString(issue_or_pr.Author.Name)
	if len(issue_or_pr.Description) > 0 {
		sb.WriteString(": ")
		sb.WriteString(issue_or_pr.Description)
	}

	if len(issue_or_pr.Labels) > 0 {
		sb.WriteString("\n\nLabels: ")
		sb.WriteString(strings.Join(issue_or_pr.Labels, ", "))
	}

	if issue_or_pr.UserNotesCount <= 0 {
		return sb.String(), nil
	}

	options := &gitlab.ListIssueNotesOptions{
		ListOptions: gitlab.ListOptions{
			Page:    1,
			PerPage: perPageEntries,
		},
		Sort:    gitlab.Ptr("asc"),
		OrderBy: gitlab.Ptr("created_at"),
	}

	for {
		notes, resp, err := client.Notes.ListIssueNotes(issue_or_pr.ProjectID, issue_or_pr.IID, options)
		if err != nil {
			return "", err
		}

		for _, note := range notes {
			sb.WriteString("\n\n")
			sb.WriteString(note.Author.Name)
			sb.WriteString(": ")
			sb.WriteString(note.Body)
		}

		if resp.CurrentPage >= resp.TotalPages {
			break
		}

		options.Page = resp.NextPage
	}

	return sb.String(), nil
}

func extractAllTextFromMergeRequest(client *gitlab.Client, mergeRequest *gitlab.BasicMergeRequest) (string, error) {
	var sb strings.Builder

	sb.WriteString(mergeRequest.Author.Name)
	sb.WriteString(": ")
	sb.WriteString(mergeRequest.Description)

	if mergeRequest.UserNotesCount <= 0 {
		return sb.String(), nil
	}

	options := &gitlab.ListMergeRequestNotesOptions{
		ListOptions: gitlab.ListOptions{
			Page:    1,
			PerPage: perPageEntries,
		},
		Sort:    gitlab.Ptr("asc"),
		OrderBy: gitlab.Ptr("created_at"),
	}

	for {
		notes, resp, err := client.Notes.ListMergeRequestNotes(mergeRequest.ProjectID, mergeRequest.IID, options)
		if err != nil {
			return "", err
		}

		for _, note := range notes {
			sb.WriteString("\n\n")
			sb.WriteString(note.Author.Name)
			sb.WriteString(": ")
			sb.WriteString(note.Body)
		}

		if resp.CurrentPage >= resp.TotalPages {
			break
		}

		options.Page = resp.NextPage
	}

	return sb.String(), nil
}

func listAllProjectIssues(client *gitlab.Client, projectPath string, outputChannel chan any) error {
	options := &gitlab.ListProjectIssuesOptions{
		ListOptions: gitlab.ListOptions{
			Page:    1,
			PerPage: perPageEntries,
		},
		Confidential: gitlab.Ptr(false),
	}

	for {
		issues, resp, err := client.Issues.ListProjectIssues(projectPath, options)
		if err != nil {
			return err
		}

		for _, issue := range issues {
			outputChannel <- issue
		}

		if resp.CurrentPage >= resp.TotalPages {
			break
		}

		options.Page = resp.NextPage
	}

	return nil
}

func listAllProjectMergeRequests(client *gitlab.Client, projectPath string, outputChannel chan any) error {
	options := &gitlab.ListProjectMergeRequestsOptions{
		ListOptions: gitlab.ListOptions{
			Page:    1,
			PerPage: perPageEntries,
		},
	}

	for {
		mergeRequests, resp, err := client.MergeRequests.ListProjectMergeRequests(projectPath, options)
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
