package scrapers

import "testing"

func Test_stripHTMLTags(t *testing.T) {
	tests := []struct {
		arg      string
		wantText string
		wantErr  bool
	}{
		{"<p>Hello, World!</p>", "Hello, World!", false},
		{"<div><h1>Title</h1><p>Content</p></div>", "Title\nContent", false},
		{"<script>alert('test');</script><p>Text</p>", "Text", false},
		{"Text", "Text", false},
		{"This is a\n\nmore complex \n\nmultiline text.", "This is a\nmore complex \nmultiline text.", false},
	}
	for _, tt := range tests {
		t.Run(t.Name(), func(t *testing.T) {
			gotText, err := stripHTMLTags(tt.arg)
			if (err != nil) != tt.wantErr {
				t.Errorf("stripHTMLTags() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if gotText != tt.wantText {
				t.Errorf("stripHTMLTags() = %v, want %v", gotText, tt.wantText)
			}

			// Stripping again should yield the same result
			gotText2, err := stripHTMLTags(gotText)
			if err != nil {
				t.Errorf("stripHTMLTags() on stripped text error = %v", err)
				return
			}
			if gotText2 != tt.wantText {
				t.Errorf("stripHTMLTags() on stripped text = %v, want %v", gotText2, tt.wantText)
			}
		})
	}
}
