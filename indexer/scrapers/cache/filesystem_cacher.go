package cache

import (
	"bufio"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

type FilesystemCacher struct {
	cacheBaseDir    string
	originalBaseDir string
}

var _ fs.FS = &FilesystemCacher{}

// NewCacher creates a new Cacher.
func NewCacher(cacheBaseDir, jobName string, originalBaseDir string) *FilesystemCacher {
	return &FilesystemCacher{
		cacheBaseDir:    filepath.Join(cacheBaseDir, jobName),
		originalBaseDir: originalBaseDir,
	}
}

func (c *FilesystemCacher) Open(relPath string) (f fs.File, err error) {
	var originalPath = filepath.Join(c.originalBaseDir, relPath)
	origStat, err := os.Stat(originalPath)
	if err != nil {
		return
	}

	var cachePath = filepath.Join(c.cacheBaseDir, relPath)
	cachedStat, err := os.Stat(cachePath)
	if err != nil {
		if os.IsNotExist(err) {
			return c.openAndCache(cachePath, originalPath)
		}
		return
	}

	// Check if the original file is newer or different size
	if origStat.ModTime().After(cachedStat.ModTime()) || origStat.Size() != cachedStat.Size() {
		return c.openAndCache(cachePath, originalPath)
	}

	// Up to date
	return os.Open(cachePath)
}

type CachedFile struct {
	originalFile      *os.File
	cacheFile         *os.File
	bufferedCacheFile *bufio.Writer

	tr io.Reader
}

var _ fs.File = &CachedFile{}

func (f *CachedFile) Stat() (fs.FileInfo, error) {
	return f.originalFile.Stat()
}

func (f *CachedFile) Read(p []byte) (n int, err error) {
	return f.tr.Read(p)
}

func (f *CachedFile) Close() error {
	err := f.bufferedCacheFile.Flush()
	if err != nil {
		return err
	}
	err = f.cacheFile.Close()
	if err != nil {
		return err
	}

	// Now set the cache file's mod time to the original file's mod time
	originalStat, err := f.originalFile.Stat()
	if err != nil {
		return err
	}

	err = os.Chtimes(f.cacheFile.Name(), originalStat.ModTime(), originalStat.ModTime())
	if err != nil {
		return err
	}

	return f.originalFile.Close()
}

func (c *FilesystemCacher) openAndCache(cachePath string, originalPath string) (f fs.File, err error) {
	originalFile, err := os.Open(originalPath)
	if err != nil {
		return
	}

	err = os.MkdirAll(filepath.Dir(cachePath), 0755)
	if err != nil {
		originalFile.Close()
		return
	}

	cacheFile, err := os.Create(cachePath)
	if err != nil {
		originalFile.Close()
		return
	}

	bufferedCacheFile := bufio.NewWriterSize(cacheFile, 1*1024*1024)

	return &CachedFile{
		originalFile:      originalFile,
		cacheFile:         cacheFile,
		bufferedCacheFile: bufferedCacheFile,
		tr:                io.TeeReader(originalFile, bufferedCacheFile),
	}, nil
}
