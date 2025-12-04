package extractors

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

var testFS = fstest.MapFS{
	"test.txt": &fstest.MapFile{
		Data:    []byte("This is a test file."),
		Mode:    fs.ModePerm,
		ModTime: time.Now(),
	},
	"test.pdf": &fstest.MapFile{
		// pdf header bytes
		Data:    []byte("%PDF-1.4\n%âãÏÓ\n1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n"),
		Mode:    fs.ModePerm,
		ModTime: time.Now(),
	},
	"test.asm": &fstest.MapFile{
		Data:    []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
		Mode:    fs.ModePerm,
		ModTime: time.Now(),
	},
	// Typical code files
	"test.go": &fstest.MapFile{
		Data:    []byte("// This is a Go test file\npackage main\nfunc main() {}\n"),
		Mode:    fs.ModePerm,
		ModTime: time.Now(),
	},
	"test.py": &fstest.MapFile{
		Data:    []byte("# This is a Python test file\nprint('Hello, World!')\n"),
		Mode:    fs.ModePerm,
		ModTime: time.Now(),
	},
	"test.c": &fstest.MapFile{
		Data:    []byte("// This is a C test file\n#include <stdio.h>\nint main() { return 0; }\n"),
		Mode:    fs.ModePerm,
		ModTime: time.Now(),
	},
	"test.json": &fstest.MapFile{
		Data:    []byte(`{"key": "value"}`),
		Mode:    fs.ModePerm,
		ModTime: time.Now(),
	},
	"test.xml": &fstest.MapFile{
		Data:    []byte(`<root><element>value</element></root>`),
		Mode:    fs.ModePerm,
		ModTime: time.Now(),
	},
	"test.dat": &fstest.MapFile{
		Data:    []byte{0xbb, 0xcc, 0x5b, 0xb9, 0x9c, 0xc3, 0x85, 0x3a, 0x9b, 0x1e, 0x3c, 0xba, 0xf1, 0xb3, 0x6e, 0xba},
		Mode:    fs.ModePerm,
		ModTime: time.Now(),
	},
}

func TestSimpleDocumentExtractor_ExtractText(t *testing.T) {
	extractor := &SimpleDocumentExtractor{}

	tests := []struct {
		relativePath      string
		wantExtractedText string
		wantErr           bool
	}{
		{
			"test.txt",
			"This is a test file.",
			false,
		},
		{
			"test.pdf",
			"",
			true,
		},
		{
			"test.asm",
			"",
			true,
		},
		{
			"test.go",
			"// This is a Go test file\npackage main\nfunc main() {}",
			false,
		},
		{
			"test.py",
			"# This is a Python test file\nprint('Hello, World!')",
			false,
		},
		{
			"test.c",
			"// This is a C test file\n#include <stdio.h>\nint main() { return 0; }",
			false,
		},
		{
			"test.json",
			`{"key": "value"}`,
			false,
		},
		{
			"test.xml",
			`<root><element>value</element></root>`,
			false,
		},
		{
			"test.dat",
			"",
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.relativePath, func(t *testing.T) {
			gotExtractedText, err := extractor.ExtractText(testFS, tt.relativePath)
			if (err != nil) != tt.wantErr {
				t.Errorf("SimpleDocumentExtractor.ExtractText(%v) error = %v, wantErr %v", tt.relativePath, err, tt.wantErr)
				return
			}
			if strings.TrimSpace(gotExtractedText) != strings.TrimSpace(tt.wantExtractedText) {
				t.Errorf("SimpleDocumentExtractor.ExtractText(%v) = %q, want %q", tt.relativePath, gotExtractedText, tt.wantExtractedText)
			}
		})
	}
}
