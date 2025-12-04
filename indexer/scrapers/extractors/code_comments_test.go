package extractors

import (
	"reflect"
	"strings"
	"testing"
)

func TestExtractCodeComments(t *testing.T) {
	type args struct {
		fp   string
		code string
	}
	tests := []struct {
		args         args
		wantComments []string
		wantErr      bool
	}{
		{
			args: args{
				fp:   "gocloc.go",
				code: "package gocloc\n\n// This is a test comment\nfunc test() {\n\t// This is a test2 comment\n\tfmt.Println(\"Hello, World!\")\n}",
			},
			wantComments: []string{"// This is a test comment", "// This is a test2 comment"},
			wantErr:      false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.args.fp, func(t *testing.T) {
			gotComments, err := ExtractCodeComments(tt.args.fp, strings.NewReader(tt.args.code))
			if (err != nil) != tt.wantErr {
				t.Errorf("ExtractCodeComments() error = %#v, wantErr %#v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(gotComments, tt.wantComments) {
				t.Errorf("ExtractCodeComments() = %#v, want %#v", gotComments, tt.wantComments)
			}
		})
	}
}
