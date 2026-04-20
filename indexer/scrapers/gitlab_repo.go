package scrapers

import (
	"context"
	"fmt"
	"indexer/scrapers/cache"
	"indexer/scrapers/extractors"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"shared/config"
	"shared/doc"
	"shared/embedding"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/google/go-tika/tika"
	"github.com/meilisearch/meilisearch-go"
	"github.com/saracen/walker"
)

type ScrapeGitJob struct {
	Name string
	Repo config.Repository

	GitLabID int64

	// Forge is the kind of hosting platform the repo is served from. Zero
	// value means "gitlab" (back-compat with every existing call site).
	// Supported: "", "gitlab", "gitea", "github". Controls whether Setup
	// performs the GitLab project lookup and which file-URL shape Run uses.
	Forge string

	TikaClient *tika.Client
	Index      meilisearch.IndexManager

	Embedder *embedding.EmbedConfig

	Config *config.Config

	tempDir string

	IntervalHelper
}

const (
	ForgeGitLab = "gitlab"
	ForgeGitea  = "gitea"
	ForgeGitHub = "github"
)

// isGitLabForge reports whether the job targets a GitLab host — including
// the zero-value default, which historically meant "gitlab".
func (s *ScrapeGitJob) isGitLabForge() bool {
	return s.Forge == "" || s.Forge == ForgeGitLab
}

func (s *ScrapeGitJob) DisplayName() string {
	return fmt.Sprintf("Git:%s (%s)", s.Name, s.Repo.URL)
}

func (s *ScrapeGitJob) Setup() (err error) {
	urlParsed, err := url.Parse(s.Repo.URL)
	if err != nil {
		return fmt.Errorf("%s: failed to parse repo URL: %w", s.Name, err)
	}

	if s.Config.Cache.Repos == "" {
		s.tempDir, err = os.MkdirTemp("", "git-repo-"+cleanFilename(s.Name))
		if err != nil {
			return fmt.Errorf("%s: failed to create temp dir: %w", s.Name, err)
		}
	} else {
		// Explicitly allow slashes, prevents conflicts with other repos
		s.tempDir = filepath.Join(s.Config.Cache.Repos, urlParsed.Hostname(), s.Name)
		err = os.MkdirAll(s.tempDir, 0700)
		if err != nil {
			return fmt.Errorf("%s: failed to create cache dir: %w", s.Name, err)
		}
	}

	if s.GitLabID != 0 || s.Repo.IsWiki || !s.isGitLabForge() {
		return nil
	}

	gitlabClient, err := GitLabClientFromURL(s.Config, s.Repo.URL)
	if err != nil {
		return fmt.Errorf("%s: failed to create GitLab client: %w", s.Name, err)
	}

	projectPath := strings.TrimSuffix(strings.TrimPrefix(urlParsed.Path, "/"), ".git")

	projectInfo, _, err := gitlabClient.Projects.GetProject(projectPath, nil)
	if err != nil {
		return fmt.Errorf("%s: failed to get project info: %w", s.Name, err)
	}

	s.GitLabID = int64(projectInfo.ID)

	return nil
}

func (s *ScrapeGitJob) Run() (err error) {
	logger := log.New(os.Stdout, fmt.Sprintf("[ScrapeGitJob:%s] ", s.Name), log.LstdFlags)
	logger.Printf("Scraping %s", s.Name)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var expirationChecker = NewExpirationChecker(ctx, s.Embedder, logger, s.Index, s.Config.DeleteAfterTime, true)

	repoUrl, err := url.ParseRequestURI(s.Repo.URL)
	if err != nil {
		return fmt.Errorf("%s: failed to parse repo URL: %w", s.Name, err)
	}

	// Clone the latest version of the repo
	tokenUrl := urlWithAuthToken(s.Config, repoUrl).String()

	var onFail = func() {
		// If we fail completely, remove the temp dir to make sure it gets re-cloned next time
		_ = os.RemoveAll(s.tempDir)
		_ = os.MkdirAll(s.tempDir, 0700)
	}

	if _, err := os.Stat(filepath.Join(s.tempDir, ".git")); os.IsNotExist(err) {
		_, err = runCommandCtx(ctx, false, "git", "--no-pager", "clone", tokenUrl, s.tempDir)
		if err != nil {
			onFail()
			return fmt.Errorf("%s: failed to clone repo: %w", s.Name, err)
		}
	} else {
		_, err = runCommandCtx(ctx, false, "git", "--no-pager", "-C", s.tempDir, "fetch", "origin")
		if err != nil {
			onFail()
			return fmt.Errorf("%s: failed to fetch repo: %w", s.Name, err)
		}
		branch, err := runCommandCtx(ctx, true, "git", "--no-pager", "-C", s.tempDir, "symbolic-ref", "refs/remotes/origin/HEAD")
		if err != nil {
			onFail()
			return fmt.Errorf("%s: failed to get default branch: %w", s.Name, err)
		}
		branch = strings.TrimSpace(strings.TrimPrefix(branch, "refs/remotes/origin/"))
		_, err = runCommandCtx(ctx, false, "git", "--no-pager", "-C", s.tempDir, "checkout", branch)
		if err != nil {
			onFail()
			return fmt.Errorf("%s: failed to checkout branch: %w", s.Name, err)
		}
		_, err = runCommandCtx(ctx, false, "git", "--no-pager", "-C", s.tempDir, "reset", "--hard", "origin/"+branch)
		if err != nil {
			onFail()
			return fmt.Errorf("%s: failed to reset branch: %w", s.Name, err)
		}
	}

	branchName, err := runCommandCtx(ctx, true, "git", "--no-pager", "-C", s.tempDir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		onFail()
		return fmt.Errorf("%s: failed to get branch name: %w", s.Name, err)
	}

	wikiURL, err := wikiBaseUrlFromClone(s.Repo.URL)
	if err != nil && s.Repo.IsWiki {
		return err
	}

	type documentJob struct {
		relPath             string
		docID               string
		fullURL             string
		slug                string
		modificationTimeInt int64
		ex                  *cache.CachedTextExtractor
	}
	var (
		fileCountLock sync.Mutex
		fileCount     int
		start         = time.Now()

		documentBatcher = doc.NewDocumentBatcher(s.Index, s.Embedder, s.Config.MaxDocumentSize, doc.MEILISEARCH_SAFE_MAX_COMMIT_SIZE, s.Config.MinCommit, logger)
		dirFS           = os.DirFS(s.tempDir)

		jobChan = make(chan documentJob, s.Config.MinCommit)
		wg      sync.WaitGroup
	)

	var addDocument = func(relPath string, doc doc.Document) {
		numCommitted, err := documentBatcher.AddDocument(doc)
		if err != nil {
			logger.Printf("Failed to add files %s: %v", relPath, err)
			return
		}
		if numCommitted > 0 {
			logger.Printf("Committed %d files", numCommitted)
		}

		logger.Printf("Staged file %s (%s)", relPath, humanize.Bytes(uint64(len(doc.Content))))
		if !doc.ReIndex {
			fileCountLock.Lock()
			fileCount++
			fileCountLock.Unlock()
		}
	}

	for i := 0; i < s.Config.TextWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobChan {
				logger.Printf("%s: Extracting text from %s", s.Name, job.relPath)

				content, err := job.ex.ExtractText(dirFS, job.relPath, &job.modificationTimeInt)
				if err != nil {
					logger.Printf("Failed to extract text from %s: %v", job.relPath, err)
				}

				var document = doc.Document{
					ID:            job.docID,
					URL:           job.fullURL,
					Content:       content,
					Slug:          job.slug,
					Title:         markdownGetTitle(content, job.relPath),
					LastModified:  doc.FloatyNumber(job.modificationTimeInt),
					ReIndex:       extractors.ShouldReindex(err),
					Version:       doc.DocumentVersion,
					IsCode:        extractors.IsCodeFile(job.relPath),
					Extractor:     job.ex.Name(),
					PermissionTag: s.Repo.PermissionTag,
				}

				addDocument(job.relPath, document)
			}
		}()
	}

	var totalCheckCount atomic.Int64
	err = walker.Walk(s.tempDir, func(path string, info os.FileInfo) error {
		if info.IsDir() || strings.HasPrefix(filepath.Base(path), ".") {
			if info.IsDir() && strings.Contains(path, ".git") {
				return filepath.SkipDir
			}
			return nil
		}

		relPath, err := filepath.Rel(s.tempDir, path)
		if err != nil {
			return err
		}
		relPath = strings.ReplaceAll(relPath, "\\", "/")

		var slug = s.Name + "/" + relPath

		var url string
		if s.Repo.IsWiki {
			url, err = pathToWikiURL(wikiURL, relPath)
		} else {
			url, err = repoFilePathToURL(s.Forge, repoUrl.String(), string(branchName), relPath)
		}
		if err != nil {
			logger.Printf("Failed to get URL for %s: %v", path, err)
			return nil
		}

		// Get git file modification timestamp
		modificationTime, err := runCommandCtx(ctx, true, "git", "--no-pager", "-C", s.tempDir, "log", "-1", "--format=%ct", "--", relPath)
		if err != nil {
			logger.Printf("Failed to get modification time for %s: %v", relPath, err)
			return nil
		}
		modificationTimeInt, err := strconv.ParseInt(strings.TrimSpace(modificationTime), 10, 64)
		if err != nil {
			logger.Printf("Failed to parse modification time for %s: %v", relPath, err)
			return nil
		}

		var docID = hashURL(url)

		extractor, err := cache.NewCachedTextExtractor(logger, s.Config.Cache.TextDir, s.Name, extractors.FromPath(relPath, s.TikaClient))
		if err != nil {
			logger.Printf("Failed to create text extractor for %s: %v", relPath, err)
			return nil
		}

		totalCheckCount.Add(1)
		exist, reason, err := expirationChecker.IsDocUpToDate(docID, modificationTimeInt, extractor.Name(), s.Repo.PermissionTag)
		if err == nil && exist {
			return nil
		}

		if TextExtractionAllowed(Cascade(s.Repo.Include, s.Config.DefaultIncludes), Cascade(s.Repo.Exclude, s.Config.DefaultExcludes), relPath) {
			logger.Printf("Queueing %s for extraction (%s)", relPath, reason)
			jobChan <- documentJob{
				relPath:             relPath,
				docID:               docID,
				fullURL:             url,
				slug:                slug,
				modificationTimeInt: modificationTimeInt,
				ex:                  extractor,
			}
		} else {
			addDocument(relPath, doc.Document{
				ID:            docID,
				URL:           url,
				Content:       "",
				Slug:          slug,
				Title:         markdownGetTitle("", relPath),
				LastModified:  doc.FloatyNumber(modificationTimeInt),
				ReIndex:       false,
				Version:       doc.DocumentVersion,
				IsCode:        extractors.IsCodeFile(relPath),
				Extractor:     extractor.Name(),
				PermissionTag: s.Repo.PermissionTag,
			})
		}

		return nil
	}, walker.WithLimit(4))
	logger.Println("Waiting for text extraction workers to finish")
	cancel()
	close(jobChan)
	wg.Wait()
	if err != nil {
		return fmt.Errorf("%s: failed to walk repo: %w", s.Name, err)
	}

	numCommitted, err := documentBatcher.Commit()
	if err != nil {
		return fmt.Errorf("%s: failed to commit documents: %w", s.Name, err)
	}
	if numCommitted > 0 {
		logger.Printf("Committed %d documents", numCommitted)
	}

	logger.Printf("Scraped %d and checked %d files in %s", fileCount, totalCheckCount.Load(), time.Since(start))

	return
}

// Turn e.g. "https://gitlab.example.com/mygroup.wiki.git" into "https://gitlab.example.com/groups/mygroup/-/wikis"
func wikiBaseUrlFromClone(cloneURL string) (wikiWebLink string, err error) {
	u, err := url.Parse(cloneURL)
	if err != nil {
		return
	}
	u.Path = strings.TrimSuffix(u.Path, ".git")
	parts := strings.Split(u.Path, ".")
	if len(parts) != 2 {
		err = fmt.Errorf("invalid wiki clone URL")
		return
	}

	u.Path = fmt.Sprintf("/groups%s/-/wikis", parts[0])
	return u.String(), nil
}

func gitlabFilePathToURL(repoBaseURL, branchName, p string) (out string, err error) {
	// Go from repo url + branch name + file path to the URL of the file
	url, err := url.Parse(strings.TrimSuffix(repoBaseURL, ".git"))
	if err != nil {
		return
	}

	url.Path = strings.ReplaceAll(filepath.Join(url.Path, "-", "blob", branchName, p), "\\", "/")

	return url.String(), nil
}

// giteaFilePathToURL builds a Gitea file web URL. Shape:
//
//	<host>/<owner>/<repo>/src/branch/<branch>/<path>
func giteaFilePathToURL(repoBaseURL, branchName, p string) (out string, err error) {
	u, err := url.Parse(strings.TrimSuffix(repoBaseURL, ".git"))
	if err != nil {
		return
	}
	u.Path = strings.ReplaceAll(filepath.Join(u.Path, "src", "branch", branchName, p), "\\", "/")
	return u.String(), nil
}

// githubFilePathToURL builds a GitHub file web URL. Shape:
//
//	<host>/<owner>/<repo>/blob/<branch>/<path>
func githubFilePathToURL(repoBaseURL, branchName, p string) (out string, err error) {
	u, err := url.Parse(strings.TrimSuffix(repoBaseURL, ".git"))
	if err != nil {
		return
	}
	u.Path = strings.ReplaceAll(filepath.Join(u.Path, "blob", branchName, p), "\\", "/")
	return u.String(), nil
}

// repoFilePathToURL dispatches to the right per-forge URL builder.
func repoFilePathToURL(forge, repoBaseURL, branchName, p string) (string, error) {
	switch forge {
	case ForgeGitea:
		return giteaFilePathToURL(repoBaseURL, branchName, p)
	case ForgeGitHub:
		return githubFilePathToURL(repoBaseURL, branchName, p)
	default:
		return gitlabFilePathToURL(repoBaseURL, branchName, p)
	}
}

func pathToWikiURL(repoBaseURL, relPath string) (out string, err error) {
	// Basically turn something like my_group.wiki, https://gitlab.example.com/groups/my_group/-/wikis/, my_group.wiki/SomeDir/Document.md
	// into https://gitlab.example.com/groups/my_group/-/wikis/SomeDir/Document.md
	return strings.TrimSuffix(repoBaseURL, "/") + "/" + relPath, nil
}

func urlWithAuthToken(cfg *config.Config, cloneURL *url.URL) *url.URL {
	// Clone the URL
	newURL := *cloneURL

	// If we have a token for this host, add it to the new URL
	for _, host := range cfg.GitLogins {
		if host.Host == cloneURL.Host {
			newURL.User = url.UserPassword("oauth2", host.Token)
			return &newURL
		}
	}

	return &newURL
}
