package cache

import (
	"context"
	"encoding/json"
	"indexer/scrapers/extractors"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
)

type CachedTextExtractor struct {
	log     *log.Logger
	baseDir string
	jobName string
	ex      extractors.Extractor
}

func NewCachedTextExtractor(logger *log.Logger, baseDir string, jobName string, ex extractors.Extractor) (c *CachedTextExtractor, err error) {
	c = &CachedTextExtractor{
		log:     logger,
		baseDir: baseDir,
		jobName: jobName,
		ex:      ex,
	}

	return c, nil
}

type cachedText struct {
	Text             string `json:"text"`
	FileLastModified int64  `json:"last_modified"`
	FileSize         int64  `json:"file_size"`
	Extractor        string `json:"extractor"`
}

func (c *CachedTextExtractor) loadCached(cachedPath string, expected fs.FileInfo, modTimeOverwrite *int64) (text string, ok bool) {
	cachedFile, err := os.Open(cachedPath)
	if err != nil {
		return "", false
	}
	defer cachedFile.Close()

	decoder, err := zstd.NewReader(cachedFile)
	if err != nil {
		return "", false
	}
	defer decoder.Close()

	var cached cachedText
	err = json.NewDecoder(decoder).Decode(&cached)
	if err != nil {
		return "", false
	}

	var expectedTime, expectedSize int64
	if expected != nil {
		expectedTime = expected.ModTime().Unix()
		expectedSize = expected.Size()
	}
	if modTimeOverwrite != nil {
		expectedTime = *modTimeOverwrite
	}

	if cached.FileLastModified != expectedTime || cached.FileSize != expectedSize || cached.Extractor != c.ex.Name() {
		return "", false
	}

	return cached.Text, true
}

func (c *CachedTextExtractor) ExtractText(dirFS fs.FS, relativePath string, modTimeOverwrite ...*int64) (text string, err error) {
	cachedPath := filepath.Join(c.baseDir, c.jobName, relativePath+".json.zst")

	stat, err := fs.Stat(dirFS, relativePath)
	if err != nil {
		return "", err
	}

	var mo *int64
	if len(modTimeOverwrite) > 0 {
		mo = modTimeOverwrite[0]
	}

	// Fast path: if the cached file is up-to-date, return it
	if cachedText, ok := c.loadCached(cachedPath, stat, mo); ok {
		c.log.Printf("Found cached text for %s", relativePath)
		return cachedText, nil
	}

	text, err = c.ex.ExtractText(dirFS, relativePath)
	if err != nil {
		return "", err
	}

	var cached = cachedText{
		Text:             text,
		FileLastModified: stat.ModTime().Unix(),
		FileSize:         stat.Size(),
		Extractor:        c.ex.Name(),
	}

	err = os.MkdirAll(filepath.Dir(cachedPath), 0755)
	if err != nil {
		return "", err
	}

	f, err := os.Create(cachedPath)
	if err != nil {
		return "", err
	}

	encoder, err := zstd.NewWriter(f)
	if err != nil {
		_ = f.Close()
		return "", err
	}

	err = json.NewEncoder(encoder).Encode(cached)
	if err != nil {
		log.Printf("Failed to encode cached text for %s: %v", relativePath, err)
		_ = encoder.Close()
		_ = f.Close()
		return "", err
	}

	err = encoder.Close()
	if err != nil {
		return "", err
	}

	err = f.Close()
	if err != nil {
		return "", err
	}

	return text, nil
}

func (c *CachedTextExtractor) ExtractTextStream(ctx context.Context, documentID string, file io.Reader) (text string, err error) {
	return c.ex.ExtractTextStream(ctx, documentID, file)
}

func (c CachedTextExtractor) Name() string {
	return c.ex.Name()
}
