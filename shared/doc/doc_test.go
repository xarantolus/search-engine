package doc

import (
	"encoding/json"
	"strconv"
	"testing"
)

func TestDocumentMarshal(t *testing.T) {
	var doc = Document{
		ID:            "abcdef",
		URL:           "http://example.com",
		Content:       "Some Custom Content",
		Slug:          "custom/slug",
		Title:         "My Title",
		LastModified:  9007199254740992,
		PermissionTag: "tag",
		IndexTime:     9007199254740993,
		Version:       DocumentVersion,
		ReIndex:       false,
		IsCode:        false,
		Extractor:     "something",
	}

	bytes, err := json.Marshal(&doc)
	if err != nil {
		t.Fatalf("Failed to marshal document: %v", err)
	}

	var expected = `{"id":"abcdef","url":"http://example.com","content":"Some Custom Content","contentSize":0,"slug":"custom/slug","title":"My Title","lastModified":9007199254740992,"version":` +
		strconv.Itoa(DocumentVersion) +
		`,"reindex":false,"isCode":false,"extractor":"something","permissionTag":"tag","indexTime":9007199254740993}`
	if string(bytes) != expected {
		t.Fatalf("Expected %q, got %q", expected, string(bytes))
	}
}
