package scrapers

import "testing"

func Test_wikiBaseUrlFromClone(t *testing.T) {
	type args struct {
		cloneURL string
	}
	tests := []struct {
		name            string
		args            args
		wantWikiWebLink string
		wantErr         bool
	}{
		{
			name:            "valid URL",
			args:            args{cloneURL: "https://gitlab.example.com/mygroup.wiki.git"},
			wantWikiWebLink: "https://gitlab.example.com/groups/mygroup/-/wikis",
			wantErr:         false,
		},
		{
			name:            "invalid URL",
			args:            args{cloneURL: "https://gitlab.example.com/mygroup"},
			wantWikiWebLink: "",
			wantErr:         true,
		},
		{
			name:            "another valid URL",
			args:            args{cloneURL: "https://gitlab.example.com/mygroup.wiki"},
			wantWikiWebLink: "https://gitlab.example.com/groups/mygroup/-/wikis",
			wantErr:         false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotWikiWebLink, err := wikiBaseUrlFromClone(tt.args.cloneURL)
			if (err != nil) != tt.wantErr {
				t.Errorf("wikiBaseUrlFromClone() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if gotWikiWebLink != tt.wantWikiWebLink {
				t.Errorf("wikiBaseUrlFromClone() = %v, want %v", gotWikiWebLink, tt.wantWikiWebLink)
			}
		})
	}
}
