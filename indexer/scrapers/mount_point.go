package scrapers

import (
	"context"
	"fmt"
	"indexer/scrapers/cache"
	"indexer/scrapers/extractors"
	"io/fs"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"shared/config"
	"shared/doc"
	"shared/embedding"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/google/go-tika/tika"
	"github.com/meilisearch/meilisearch-go"
	"github.com/saracen/walker"
)

type MountPointJob struct {
	Name       string
	MountPoint config.Mount

	TikaClient *tika.Client
	Index      meilisearch.IndexManager

	Embedder *embedding.EmbedConfig
	Config   *config.Config

	logger *log.Logger
	IntervalHelper
}

func (s *MountPointJob) DisplayName() string {
	return fmt.Sprintf("MountPoint:%s (%s)", s.Name, s.MountPoint.Dir)
}

func (s *MountPointJob) Setup() (err error) {
	s.logger = log.New(os.Stdout, fmt.Sprintf("[MountPointJob:%s] ", s.Name), log.LstdFlags)

	if strings.TrimSpace(s.MountPoint.Command) == "" {
		return nil
	}

	s.logger.Println("Setting up mount point")
	err = os.MkdirAll(s.MountPoint.Dir, 0755)
	if err != nil {
		return fmt.Errorf("%s: failed to create mount point: %w", s.Name, err)
	}

	// unmount in case it was already mounted
	_, err = runCommand(true, "umount", s.MountPoint.Dir)
	if err != nil {
		s.logger.Printf("Failed to unmount %s: %v", s.MountPoint.Dir, err)
	}

	_, err = runCommandString(false, s.MountPoint.Command, true)
	if err != nil {
		return fmt.Errorf("%s: running mount command: %w", s.Name, err)
	}

	return nil
}

func relativePathToURL(urlTransform string, relativePath string) string {
	return strings.NewReplacer(
		"{:relative:}", relativePath,
		"{:relative_query:}", url.QueryEscape(relativePath),
		"{:relative_url_path:}", url.PathEscape(relativePath),
		"{:relative_url_path_with_slash:}", strings.ReplaceAll(url.PathEscape(relativePath), "%2F", "/"),
		"{:relative_dir:}", filepath.Dir(relativePath),
		"{:relative_dir_query:}", url.QueryEscape(filepath.Dir(relativePath)),
		"{:relative_dir_url_path:}", url.PathEscape(filepath.Dir(relativePath)),
		"{:relative_dir_url_path_with_slash:}", strings.ReplaceAll(url.PathEscape(filepath.Dir(relativePath)), "%2F", "/"),
		"{:relative_file:}", filepath.Base(relativePath),
		"{:relative_file_query:}", url.QueryEscape(filepath.Base(relativePath)),
		"{:relative_file_url_path:}", url.PathEscape(filepath.Base(relativePath)),
		"{:relative_file_url_path_with_slash:}", strings.ReplaceAll(url.PathEscape(filepath.Base(relativePath)), "%2F", "/"),
	).Replace(urlTransform)
}

func (s *MountPointJob) Run() (err error) {
	s.logger.Printf("Running mount point job for %s", s.MountPoint.Dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var expirationChecker = NewExpirationChecker(ctx, s.Embedder, s.logger, s.Index, s.Config.DeleteAfterTime, true)

	var (
		fileCountLock   sync.Mutex
		fileCount       int
		start           = time.Now()
		documentBatcher = doc.NewDocumentBatcher(s.Index, s.Embedder, s.Config.MaxDocumentSize, doc.MEILISEARCH_SAFE_MAX_COMMIT_SIZE, s.Config.MinCommit, s.logger)

		fs fs.FS
	)
	if s.MountPoint.DisableCache {
		fs = os.DirFS(s.MountPoint.Dir)
	} else {
		fs = cache.NewCacher(s.Config.Cache.Dir, s.Name, s.MountPoint.Dir)
	}

	// We parallelize scanning (which takes basically no resources, but can be slow)
	// and indexing (which is CPU-bound, which is why we only do one at a time)
	type documentJob struct {
		relPath          string
		docID            string
		fullURL          string
		inFolderURL      string
		modificationTime int64
		ex               *cache.CachedTextExtractor
	}
	var (
		jobChan = make(chan documentJob, s.Config.MinCommit)
		wg      sync.WaitGroup
	)

	var addDocument = func(relPath string, doc doc.Document) {
		numCommitted, err := documentBatcher.AddDocument(doc)
		if err != nil {
			s.logger.Printf("Failed to add files %s: %v", relPath, err)
			return
		}
		if numCommitted > 0 {
			s.logger.Printf("Committed %d files", numCommitted)
		}

		s.logger.Printf("Staged file %s (%s)", relPath, humanize.Bytes(uint64(len(doc.Content))))
		if !doc.ReIndex {
			fileCountLock.Lock()
			fileCount++
			fileCountLock.Unlock()
		}
	}

	// Background workers that extract text
	for i := 0; i < s.Config.TextWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobChan {
				s.logger.Printf("Extracting text from %s", job.relPath)
				content, err := job.ex.ExtractText(fs, job.relPath)
				if err != nil {
					s.logger.Printf("Failed to extract text from %s: %v", job.relPath, err)
					// we index the file anyways, as its title can already be useful
				}

				var document = doc.Document{
					ID:            job.docID,
					URL:           job.fullURL,
					Content:       content,
					Slug:          s.Name + "/" + job.relPath,
					Title:         markdownGetTitle(content, job.relPath),
					LastModified:  doc.FloatyNumber(job.modificationTime),
					InFolderURL:   job.inFolderURL,
					ReIndex:       extractors.ShouldReindex(err),
					Version:       doc.DocumentVersion,
					IsCode:        extractors.IsCodeFile(job.relPath),
					Extractor:     job.ex.Name(),
					PermissionTag: s.MountPoint.PermissionTag,
				}

				addDocument(job.relPath, document)
			}
		}()
	}

	var errorTimestamps []time.Time
	var errorLock sync.Mutex
	var totalCheckCount atomic.Int64
	walkErr := walker.Walk(s.MountPoint.Dir, func(path string, info os.FileInfo) error {
		if strings.HasPrefix(filepath.Base(path), ".") {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			return nil
		}

		relPath, err := filepath.Rel(s.MountPoint.Dir, path)
		if err != nil {
			return err
		}
		relPath = strings.ReplaceAll(relPath, "\\", "/")

		// Check if the file is excluded via the PathFilter
		if s.MountPoint.PathFilter != "" && !AnyGlobMatches(s.MountPoint.PathFilter, relPath) {
			return nil
		}

		var fullURL = relativePathToURL(s.MountPoint.URLTransform, relPath)
		var inFolderURL string
		if s.MountPoint.InFolderURLTransform != "" {
			inFolderURL = relativePathToURL(s.MountPoint.InFolderURLTransform, relPath)
		}

		modificationTime := info.ModTime().Unix()
		if modificationTime <= 0 {
			s.logger.Printf("Skipping %s: invalid modification time", relPath)
			return nil
		}

		var docID = hashURL(fullURL)

		extractor, err := cache.NewCachedTextExtractor(s.logger, s.Config.Cache.TextDir, s.Name, extractors.FromPath(relPath, s.TikaClient))
		if err != nil {
			s.logger.Printf("Failed to create text extractor for %s: %v", relPath, err)
			return nil
		}

		// Look up the document, and if it exists and same modtime, skip
		totalCheckCount.Add(1)
		exist, reason, err := expirationChecker.IsDocUpToDate(docID, modificationTime, extractor.Name(), s.MountPoint.PermissionTag)
		if err == nil && exist {
			return nil
		}

		if TextExtractionAllowed(Cascade(s.MountPoint.Include, s.Config.DefaultIncludes), Cascade(s.MountPoint.Exclude, s.Config.DefaultExcludes), relPath) && info.Size() > 0 && info.Size() <= s.MountPoint.MaxFileSize {
			s.logger.Printf("Queueing %s for extraction (%s)", relPath, reason)
			jobChan <- documentJob{
				relPath:          relPath,
				docID:            docID,
				fullURL:          fullURL,
				inFolderURL:      inFolderURL,
				modificationTime: modificationTime,
				ex:               extractor,
			}
		} else {
			addDocument(relPath, doc.Document{
				ID:            docID,
				URL:           fullURL,
				Slug:          s.Name + "/" + relPath,
				Title:         markdownGetTitle("", relPath),
				LastModified:  doc.FloatyNumber(modificationTime),
				InFolderURL:   inFolderURL,
				ReIndex:       false,
				Version:       doc.DocumentVersion,
				IsCode:        extractors.IsCodeFile(relPath),
				Extractor:     extractor.Name(),
				Content:       "",
				PermissionTag: s.MountPoint.PermissionTag,
			})
		}

		return nil
	}, walker.WithLimit(4), walker.WithErrorCallback(func(pathname string, err error) error {
		errorLock.Lock()

		now := time.Now()
		errorTimestamps = append(errorTimestamps, now)

		// Remove errors older than one minute
		oneMinuteAgo := now.Add(-time.Minute)
		n := 0
		for _, t := range errorTimestamps {
			if t.After(oneMinuteAgo) {
				errorTimestamps[n] = t
				n++
			}
		}
		errorTimestamps = errorTimestamps[:n]
		var count = len(errorTimestamps)
		errorLock.Unlock()

		s.logger.Printf("Error walking %s: %v. Errors in last minute: %d", pathname, err, count)

		// Wait a little bit to recover from errors
		if len(errorTimestamps) > 10 {
			time.Sleep(time.Duration(count) * time.Second)
		}
		return nil
	}))
	s.logger.Println("Waiting for text extraction workers to finish")
	cancel()
	close(jobChan)
	wg.Wait()

	numCommitted, err := documentBatcher.Commit()
	if err != nil {
		return fmt.Errorf("%s: failed to commit files: %w", s.Name, err)
	}
	if numCommitted > 0 {
		s.logger.Printf("Committed %d files", numCommitted)
	}

	s.logger.Printf("Scraped %d and checked %d files in %s", fileCount, totalCheckCount.Load(), time.Since(start))

	if walkErr != nil {
		return fmt.Errorf("%s: failed to walk repo: %w", s.Name, walkErr)
	}

	return nil
}
