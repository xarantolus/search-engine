package extractors

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-tika/tika"
)

var (
	// This error is used to indicate that extraction should not retried on this file
	ErrNonRetryable = errors.New("non-retryable error")
)

func ShouldReindex(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, ErrNonRetryable) {
		return false
	}

	return true
}

type Extractor interface {
	ExtractText(dirFS fs.FS, relativePath string) (string, error)
	ExtractTextStream(ctx context.Context, docID string, file io.Reader, contentType ...string) (string, error)
	Name() string
}

type CodeExtractor struct{}

func (c *CodeExtractor) ExtractText(dirFS fs.FS, relativePath string) (string, error) {
	comments, err := ExtractCodeCommentsFile(dirFS, relativePath)
	return strings.Join(comments, "\n"), err
}
func (c *CodeExtractor) Name() string {
	return "code"
}

type AnyDocumentExtractor struct {
	tikaClient *tika.Client
}

func (a *AnyDocumentExtractor) ExtractText(dirFS fs.FS, relativePath string) (extractedText string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 130*time.Second)
	defer cancel()

	// Try to extract text using Tika first, with 70% of the default timeout
	dl, ok := ctx.Deadline()
	if !ok {
		dl = time.Now().Add(130 * time.Second)
	}
	tikaCtx, tikaCancel := context.WithTimeout(ctx, time.Until(dl)*7/10)
	defer tikaCancel()

	extractedText, err = tikaExtractText(tikaCtx, a.tikaClient, dirFS, relativePath, false)
	if err == nil {
		return extractedText, nil
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		log.Printf("[Tika] recursive extraction of %q failed: %v", relativePath, err)
	}

	// Check if it's a PDF and try PDF extraction
	if strings.HasSuffix(strings.ToLower(relativePath), ".pdf") {
		extractedText, err = pdfToText(ctx, dirFS, relativePath)
		if err == nil {
			return extractedText, nil
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			log.Printf("[PDF] Extraction of %q failed: %v", relativePath, err)
		}
	}

	// Try non-recursive Tika extraction
	extractedText, err = tikaExtractText(ctx, a.tikaClient, dirFS, relativePath, true)
	if err == nil {
		return extractedText, nil
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		log.Printf("[Tika] extraction without recursion failed for %q: %v", relativePath, err)
	}

	// Fall back to simple document extractor
	return (&SimpleDocumentExtractor{}).ExtractText(dirFS, relativePath)
}

func (a *AnyDocumentExtractor) ExtractTextStream(ctx context.Context, docID string, file io.Reader, contentType ...string) (extractedText string, err error) {
	// Try to extract text using Tika first, with 70% of the default timeout
	dl, ok := ctx.Deadline()
	if !ok {
		dl = time.Now().Add(60 * time.Second)
	}
	tikaCtx, cancel := context.WithTimeout(ctx, time.Until(dl)*7/10)
	defer cancel()

	extractedText, err = tikaExtractTextStream(tikaCtx, a.tikaClient, file, false, contentType...)
	if err == nil {
		return extractedText, nil
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		log.Printf("[Tika] extraction of %q failed: %v", docID, err)
	}

	// if we don't have a content type, try to sniff it
	if seeker, ok := file.(io.Seeker); ok {
		_, err = seeker.Seek(0, io.SeekStart)
		if err != nil {
			return "", fmt.Errorf("failed to seek file stream: %w", err)
		}

		if len(contentType) == 0 {
			var buf [512]byte
			_, err = io.ReadFull(file, buf[:])
			if err != nil && !errors.Is(err, io.EOF) {
				return "", fmt.Errorf("failed to read file stream for content type sniffing: %w", err)
			}

			contentType = []string{http.DetectContentType(buf[:])}
		}

		_, err = seeker.Seek(0, io.SeekStart)
		if err != nil {
			return "", fmt.Errorf("failed to seek file stream: %w", err)
		}
	} else {
		return "", fmt.Errorf("file stream is not seekable, cannot reset: %w", err)
	}

	// If we have a pdf-like content type, try to extract text using pdftotext
	if len(contentType) > 0 && strings.HasPrefix(contentType[0], "application/pdf") {
		extractedText, err = pdfToTextStream(ctx, file)
		if err == nil {
			return extractedText, nil
		}

		if !errors.Is(err, context.DeadlineExceeded) {
			log.Printf("[PDF] Extraction of %q failed: %v", docID, err)
		}
	}

	return tikaExtractTextStream(ctx, a.tikaClient, file, true, contentType...)
}

func (a *AnyDocumentExtractor) Name() string {
	return "document"
}

type SimpleDocumentExtractor struct {
}

func (a *SimpleDocumentExtractor) ExtractText(dirFS fs.FS, relativePath string) (extractedText string, err error) {
	content, err := fs.ReadFile(dirFS, relativePath)
	if err != nil {
		return "", err
	}

	// Sniff content type
	httpContentType := http.DetectContentType(content)
	if !strings.HasPrefix(httpContentType, "text/") {
		return "", fmt.Errorf("unsupported content type %s for file %s: %w", httpContentType, relativePath, ErrNonRetryable)
	}

	return string(content), nil
}

func (a *SimpleDocumentExtractor) ExtractTextStream(ctx context.Context, docID string, file io.Reader, contentType ...string) (extractedText string, err error) {
	content, err := io.ReadAll(file)
	if err != nil {
		return "", err
	}

	// Sniff content type
	httpContentType := http.DetectContentType(content)
	if !strings.HasPrefix(httpContentType, "text/") {
		return "", fmt.Errorf("unsupported content type %s for stream: %w", httpContentType, ErrNonRetryable)
	}

	return string(content), nil
}

func (a *SimpleDocumentExtractor) Name() string {
	return "simplev3"
}

func IsSimpleDocument(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".txt" || ext == ".md"
}

func FromPath(path string, tikaClient *tika.Client) Extractor {
	if IsSimpleDocument(path) || IsCodeFile(path) {
		return &SimpleDocumentExtractor{}
	}

	return &AnyDocumentExtractor{
		tikaClient: tikaClient,
	}
}

func ForStream(tikaClient *tika.Client) *AnyDocumentExtractor {
	return &AnyDocumentExtractor{
		tikaClient: tikaClient,
	}
}
