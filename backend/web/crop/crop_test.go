package crop

import (
	"reflect"
	"testing"
)

func TestExtractRelevantTerms(t *testing.T) {
	tests := []struct {
		arg  string
		want []string
	}{
		{"", []string{}},
		{" ", []string{}},
		{"hello world", []string{"hello", "world"}},
		{"hello \"world\"", []string{"hello", "world"}},
		{"\"hello world\"", []string{"hello world"}},
		{"hello -world", []string{"hello"}},
		{"\"some test\" test2", []string{"some test", "test2"}},
		{"\"quoted -term\"", []string{"quoted -term"}},
		{"-hello world", []string{"world"}},
		{"-\"quoted term\"", []string{}},
		{"-\"quoted -term\"\tabc", []string{"abc"}},
		{"hello 世界", []string{"hello", "世界"}},
		{"\"hello 世界\"", []string{"hello 世界"}},
		{"\"hello 世界\" -world", []string{"hello 世界"}},
		{"-hello 世界", []string{"世界"}},
		{"-\"hello 世界\"", []string{}},
		{"-\"hello 世界\" -world", []string{}},
		{"-\"hello 世界\" -world -test", []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.arg, func(t *testing.T) {
			if got := ExtractRelevantTerms(tt.arg, 3); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ExtractRelevantTerms(%v) = %v, want %v", tt.arg, got, tt.want)
			}
		})
	}
}

func Test_simpleFindParagraphEnding(t *testing.T) {

	tests := []struct {
		content   string
		maxLength int
		want      int
	}{
		{
			"This is the end for testing.\n\nSome additional content.",
			50,
			28, // End of "This is the end."
		},
		{
			"If we have a sentence, it might already enough info. Cut off this one.\n\nAnd this is the next paragraph.",
			55,
			52, // End of "If we have a sentennce, it might already enough info."
		},
	}
	for _, tt := range tests {
		t.Run(tt.content, func(t *testing.T) {
			if got := simpleFindParagraphEnding(tt.content, tt.maxLength); got != tt.want {
				t.Errorf("simpleFindParagraphEnding(%q, %v) = %v, want %v", tt.content, tt.maxLength, got, tt.want)

				// show when it was cut off
				t.Errorf("Content was cut to: %q", tt.content[:got])
			}
		})
	}
}

func Test_simpleFindParagraphStart(t *testing.T) {
	tests := []struct {
		content    string
		matchIndex int
		maxLength  int
		want       int
	}{
		{
			"This is the end.\n\nSome additional content.",
			12, // if "end" is at our matchindex
			50,
			0, // Start of "This is the end."
		},
		{
			"If we have a sentence, it might already enough info. Cut off this one.\n\nAnd this is the next paragraph.",
			90, // if "next" was match
			55,
			72,
		},
		{
			"If we have a sentence, it might already enough info. Cut off this one.\n\n\nAnd this is the next paragraph.",
			90, // if "next" was match
			55,
			73,
		},
	}
	for _, tt := range tests {
		t.Run(tt.content, func(t *testing.T) {
			if got := simpleFindParagraphStart(tt.content, tt.matchIndex, tt.maxLength); got != tt.want {
				t.Errorf("simpleFindParagraphStart(%q, %v, %v) = %v, want %v", tt.content, tt.matchIndex, tt.maxLength, got, tt.want)

				// show when it was cut off
				t.Errorf("Content was cut to: %q", tt.content[got:])
			}
		})
	}
}
