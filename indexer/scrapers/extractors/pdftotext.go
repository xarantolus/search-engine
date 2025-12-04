package extractors

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
)

func pdfToText(ctx context.Context, fs fs.FS, path string) (content string, err error) {
	file, err := fs.Open(path)
	if err != nil {
		return
	}
	defer file.Close()

	// Check if it is a file on the local filesystem that I can get an absolute path for
	osFile, ok := file.(*os.File)
	if !ok {
		return pdfToTextStream(ctx, file)
	}

	absPath, err := filepath.Abs(osFile.Name())
	if err != nil {
		return "", fmt.Errorf("failed to get absolute path of file %q: %w", osFile.Name(), err)
	}

	// Now we can use pdftotext
	cmd := exec.CommandContext(ctx, "pdftotext", "-nopgbrk", "-layout", absPath, "-")
	output, err := cmd.Output()
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("pdftotext failed with error: %s, output: %s", exitError.Error(), string(exitError.Stderr))
		}
		return "", fmt.Errorf("pdftotext command failed: %w", err)
	}

	return string(output), nil
}

func pdfToTextStream(ctx context.Context, file io.Reader) (string, error) {
	// Create a temporary file to hold the PDF content
	tempFile, err := os.CreateTemp("", "tempfile-*.pdf")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tempFile.Name())

	// Copy the content of the reader to the temp file
	_, err = io.Copy(tempFile, file)
	if err != nil {
		return "", fmt.Errorf("failed to copy content to temp file: %w", err)
	}
	err = tempFile.Close()
	if err != nil {
		return "", fmt.Errorf("failed to close temp file: %w", err)
	}

	return pdfToText(ctx, os.DirFS(filepath.Dir(tempFile.Name())), filepath.Base(tempFile.Name()))
}
