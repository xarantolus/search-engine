package extractors

import (
	"context"
	"os"
	"os/exec"
	"testing"
)

func Test_pdfToText(t *testing.T) {
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("pdftotext not found in PATH, skipping test")
	}

	tests := []struct {
		pdfPath     string
		wantContent string
		wantErr     bool
	}{
		{
			pdfPath:     "example.pdf",
			wantContent: "This is a test document\n\n\n\n\n    •   It contains a couple of things\n\n\n\n Like                               this   Table\n",
			wantErr:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.pdfPath, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			fs := os.DirFS("testdata")

			gotContent, err := pdfToText(ctx, fs, tt.pdfPath)
			if (err != nil) != tt.wantErr {
				t.Errorf("pdfToText() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if gotContent != tt.wantContent {
				t.Errorf("pdfToText() = %#v, want %#v", gotContent, tt.wantContent)
			}
		})
	}
}
