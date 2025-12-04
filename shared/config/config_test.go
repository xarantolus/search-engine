package config

import (
	"slices"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSynonymsMap_UnmarshalYAML_Parsing(t *testing.T) {
	tests := []struct {
		input string
		want  SynonymsMap
	}{
		{
			input: `{"a": ["b", "c"], "d": "e"}`,
			want: SynonymsMap{
				"a": {"b", "c"},
				"d": {"e"},
			},
		},
		{
			input: `{"f": "g", "h": "g"}`,
			want: SynonymsMap{
				"f": {"g"},
				"h": {"g"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			var sm SynonymsMap
			decoder := yaml.NewDecoder(strings.NewReader(tt.input))
			decoder.KnownFields(true)
			if err := decoder.Decode(&sm); err != nil {
				t.Fatalf("UnmarshalYAML error: %v", err)
			}

			if len(sm) != len(tt.want) {
				t.Errorf("got map length %d, want %d", len(sm), len(tt.want))
			}
			for key, wantSyns := range tt.want {
				gotSyns, ok := sm[key]
				if !ok {
					t.Errorf("missing key %q", key)
					continue
				}
				if !slices.Equal(gotSyns, wantSyns) {
					t.Errorf("for key %q got %v, want %v", key, gotSyns, wantSyns)
				}
			}
		})
	}
}

func TestSynonymsMap_Bidirectional(t *testing.T) {
	tests := []struct {
		input SynonymsMap
		want  SynonymsMap
	}{
		{
			input: SynonymsMap{
				"a": {"b", "c"},
				"d": {"e"},
			},
			want: SynonymsMap{
				"a": {"b", "c"},
				"b": {"a", "c"},
				"c": {"a", "b"},
				"d": {"e"},
				"e": {"d"},
			},
		},
		{
			input: SynonymsMap{
				"f": {"g"},
				"h": {"g"},
			},
			want: SynonymsMap{
				"f": {"g", "h"},
				"g": {"f", "h"},
				"h": {"g", "f"},
			},
		},
	}

	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			got := tt.input.Bidirectional()
			if len(got) != len(tt.want) {
				t.Errorf("got map length %d, want %d", len(got), len(tt.want))
			}
			for key, wantSyns := range tt.want {
				gotSyns, ok := got[key]
				if !ok {
					t.Errorf("missing key %q", key)
					continue
				}
				// sort for order-independent comparison
				gotSorted := append([]string(nil), gotSyns...)
				sort.Strings(gotSorted)
				wantSorted := append([]string(nil), wantSyns...)
				sort.Strings(wantSorted)

				if !slices.Equal(gotSorted, wantSorted) {
					t.Errorf("for key %q got %v, want %v", key, gotSorted, wantSorted)
				}
			}
		})
	}
}
