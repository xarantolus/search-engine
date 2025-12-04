package extractors

import (
	"context"
	"io"
	"io/fs"
	"net/http"
	"shared/doc"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-tika/tika"
)

// Extracts text from a file using Apache Tika.
func tikaExtractText(ctx context.Context, tikaClient *tika.Client, fs fs.FS, path string, skipEmbedded bool) (content string, err error) {
	file, err := fs.Open(path)
	if err != nil {
		return
	}
	defer file.Close()

	return tikaExtractTextStream(ctx, tikaClient, file, skipEmbedded)
}

func tikaExtractTextStream(ctx context.Context, tikaClient *tika.Client, file io.Reader, skipEmbedded bool, contentType ...string) (content string, err error) {
	var header = http.Header{
		"writeLimit": []string{strconv.Itoa(doc.MaxContentLength)},
		"Accept":     []string{"text/plain"},
	}
	if len(contentType) > 0 && len(contentType[0]) > 0 {
		header.Set("Content-Type", contentType[0])
	}
	if skipEmbedded {
		header.Set("X-Tika-Skip-Embedded", "true")
	}

	if deadline, ok := ctx.Deadline(); ok {
		// Tika will error if we submit a timeout less than its configured default timeout...
		header.Set("X-Tika-Timeout-Millis", strconv.Itoa(max(30000, int(time.Until(deadline).Milliseconds()))))
	}

	document, err := tikaClient.ParseWithHeader(ctx, file, header)
	if err != nil {
		return
	}

	return strings.TrimSpace(document), nil
}
