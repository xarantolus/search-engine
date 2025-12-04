package scrapers

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"shared/config"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/djherbis/times"
	"github.com/dustin/go-humanize"
	"github.com/saracen/walker"
)

type CacheCleanJob struct {
	FileCacheOlderThan time.Duration
	TextCacheOlderThan time.Duration

	CacheConfig config.Cache
}

func (j *CacheCleanJob) DisplayName() string {
	return "CacheCleanJob"
}

func (j *CacheCleanJob) RunInterval() time.Duration {
	return 12 * time.Hour
}

func (j *CacheCleanJob) Run() (err error) {
	if j.FileCacheOlderThan == 0 || j.TextCacheOlderThan == 0 {
		panic("CacheCleanJob: FileCacheOlderThan and TextCacheOlderThan must be set")
	}
	if j.CacheConfig.Dir == "/" || j.CacheConfig.TextDir == "/" {
		panic("CacheCleanJob: CacheConfig.Dir and CacheConfig.TextDir must not be /")
	}

	logger := log.New(os.Stdout, fmt.Sprintf("[%s]", j.DisplayName()), log.LstdFlags)

	var finalErr error
	if j.CacheConfig.Dir != "" {
		logger.Printf("Cleaning file cache older than %s", j.FileCacheOlderThan)

		avgDuration, delSize, remainingSize, err := j.deleteOlderThan(logger, j.CacheConfig.Dir, j.FileCacheOlderThan)
		if err != nil {
			logger.Printf("Failed to clean file cache: %v", err)
			finalErr = err
		}
		logger.Printf("File Cache: deleted %s, remaining %s with avgLastAccess=%s", humanize.Bytes(delSize), humanize.Bytes(remainingSize), avgDuration)
	}

	if j.CacheConfig.TextDir != "" {
		logger.Printf("Cleaning text cache older than %s", j.TextCacheOlderThan)

		avgDuration, delSize, remainingSize, err := j.deleteOlderThan(logger, j.CacheConfig.TextDir, j.TextCacheOlderThan)
		if err != nil {
			logger.Printf("Failed to clean text cache: %v", err)
			finalErr = err
		}
		logger.Printf("Text Cache: deleted %s, remaining %s with avgLastAccess=%s", humanize.Bytes(delSize), humanize.Bytes(remainingSize), &avgDuration)
	}

	return finalErr
}

// deleteOlderThan deletes files older than the given duration from the given directory, recursively.
// It will also delete all empty directories, including ones that become empty after deleting files/directories.
func (j *CacheCleanJob) deleteOlderThan(logger *log.Logger, root string, olderThan time.Duration) (avgDuration time.Duration, deletedFileSize uint64, remainingSize uint64, err error) {
	var (
		lock               sync.Mutex
		emptyCandidates    = map[string]struct{}{}
		deletedFilesSize   atomic.Uint64
		remainingCacheSize atomic.Uint64

		keptFilesCount          atomic.Int64
		totalAccessSinceSeconds atomic.Int64
	)

	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("failed to get absolute path: %w", err)
	}

	err = walker.Walk(rootAbs, func(path string, info os.FileInfo) error {
		if info.IsDir() {
			if path == rootAbs {
				return nil
			}
			lock.Lock()
			emptyCandidates[strings.TrimSuffix(path, "/")] = struct{}{}
			lock.Unlock()
			return nil
		}
		dirPath := strings.TrimSuffix(filepath.Dir(path), "/")

		accessTime := times.Get(info).AccessTime()
		if accessTime.IsZero() {
			return nil
		}

		size := uint64(info.Size())

		diff := time.Since(accessTime)
		if diff > olderThan {
			if err := os.Remove(path); err != nil {
				logger.Printf("Failed to delete file %s: %v", path, err)
				remainingCacheSize.Add(size)
			} else {
				deletedFilesSize.Add(size)

				lock.Lock()
				emptyCandidates[dirPath] = struct{}{}
				lock.Unlock()
			}
			return nil
		}

		lock.Lock()
		delete(emptyCandidates, dirPath)
		lock.Unlock()

		remainingCacheSize.Add(size)
		totalAccessSinceSeconds.Add(int64(diff.Seconds()))
		keptFilesCount.Add(1)

		return nil
	}, walker.WithLimit(4))
	avgDuration = time.Duration(int64(float64(totalAccessSinceSeconds.Load())/float64(keptFilesCount.Load())) * int64(time.Second))
	if err != nil {
		return avgDuration, deletedFilesSize.Load(), remainingCacheSize.Load(), fmt.Errorf("%s: failed to walk repo: %w", j.DisplayName(), err)
	}

	// Convert the map's keys into a slice and sort in descending order of path length
	var dirs []string
	for dir := range emptyCandidates {
		dirs = append(dirs, dir)
	}
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })

	// Attempt to remove the directories in order
	for _, dir := range dirs {
		if remErr := os.Remove(dir); remErr != nil && !strings.Contains(remErr.Error(), "not empty") {
			logger.Printf("Failed to delete directory %s: %v", dir, remErr)
		}
	}
	return avgDuration, deletedFilesSize.Load(), remainingCacheSize.Load(), nil
}
