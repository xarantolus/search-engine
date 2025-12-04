package scrapers

import (
	"context"
	"strings"
	"testing"
)

func Test_markdownGetTitle(t *testing.T) {
	tests := []struct {
		content   string
		filename  string
		wantTitle string
	}{
		{
			content:   "title: My Title\n",
			filename:  "file.md",
			wantTitle: "My Title",
		},
		{
			content: `---
title: Some History document
---
This is a page`,
			filename:  "history.md",
			wantTitle: "Some History document",
		},
		{
			content:   "Some interesting content\neven more\netc....",
			filename:  "My Presentation.md",
			wantTitle: "My Presentation",
		},
		{
			content:   "# Some Title\n",
			filename:  "file.md",
			wantTitle: "Some Title",
		},
		{
			content:   "## Some Title\n",
			filename:  "file.md",
			wantTitle: "Some Title",
		},
		{
			content:   "### Some Title\n",
			filename:  "file.md",
			wantTitle: "Some Title",
		},
		{
			content:   "#!/bin/bash\n# Some Title\n",
			filename:  "my_cool_script.sh",
			wantTitle: "my cool script",
		},
		{
			content:   "Some content",
			filename:  "MyCoolFile.md",
			wantTitle: "My Cool File",
		},
		{
			content:   "Some content",
			filename:  "MyCoolFile",
			wantTitle: "My Cool File",
		},
		{
			content:   "Some content",
			filename:  "SCREAMINGFILE.txt",
			wantTitle: "SCREAMINGFILE",
		},
		{
			content:   "Some content",
			filename:  "file.md",
			wantTitle: "file",
		},
		{
			content:   `title: 'A & B'`,
			filename:  "file.md",
			wantTitle: "A & B",
		},
	}
	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			if gotTitle := markdownGetTitle(tt.content, tt.filename); gotTitle != tt.wantTitle {
				t.Errorf("markdownGetTitle(%v, %v) = %v, want %v", tt.content, tt.filename, gotTitle, tt.wantTitle)
			}
		})
	}
}

func Test_AnyGlobMatches(t *testing.T) {
	tests := []struct {
		globs    []string
		repoName string
		want     bool
	}{
		{
			globs:    []string{"*.md"},
			repoName: "file.md",
			want:     true,
		},
		{
			globs:    []string{"*.md", "*.txt"},
			repoName: "document.txt",
			want:     true,
		},
		{
			globs:    []string{"*.md", "*.txt"},
			repoName: "image.png",
			want:     false,
		},
		{
			globs:    []string{"docs/*.md"},
			repoName: "docs/file.md",
			want:     true,
		},
		{
			globs:    []string{"docs/*.md"},
			repoName: "file.md",
			want:     false,
		},
		{
			globs:    []string{"group/subgroup/myrepo"},
			repoName: "group/subgroup/myrepo",
			want:     true,
		},
		{
			globs:    []string{"group/*/default"},
			repoName: "group/subgroup/default",
			want:     true,
		},
		{
			globs:    []string{"*/default"},
			repoName: "group/subgroup/default",
			want:     true,
		},
		{
			globs:    []string{"mygroup/*/confidential-*"},
			want:     true,
			repoName: "mygroup/subgroup/confidential-123",
		},
	}
	for _, tt := range tests {
		t.Run(tt.repoName, func(t *testing.T) {
			if got := AnyGlobMatches(strings.Join(tt.globs, ","), tt.repoName); got != tt.want {
				t.Errorf("AnyGlobMatches(%v, %v) = %v, want %v", tt.globs, tt.repoName, got, tt.want)
			}
		})
	}
}

func Test_runCommandCtxEcho(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	output, err := runCommandCtx(ctx, false, "sh", "-c", "echo Test")
	if err != nil {
		t.Fatalf("runCommandCtx() error = %v", err)
	}
	if string(output) != "Test" {
		t.Errorf("runCommandCtx() = %v, want %v", output, "Test")
	}
}

func Test_runCommandCtxGit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	output, err := runCommandCtx(ctx, false, "git", "--no-pager", "version")
	if err != nil {
		t.Fatalf("runCommandCtx() error = %v", err)
	}
	if !strings.Contains(string(output), "git version ") {
		t.Errorf("runCommandCtx() = %v, want a valid git version string", output)
	}
}
