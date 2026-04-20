package config

import (
	"slices"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestValidateAuth(t *testing.T) {
	const envSecret = "TEST_OIDC_CLIENT_SECRET"

	validOIDC := func() AuthOIDC {
		return AuthOIDC{
			IssuerURL:     "https://idp.example.com",
			ClientID:      "the-client",
			ClientSecret:  "the-secret",
			RedirectURL:   "https://app.example.com/callback",
			AllowedGroups: []string{"search-users"},
		}
	}

	tests := []struct {
		name       string
		cfg        Config
		env        map[string]string
		wantErr    string
		wantProv   string
		wantScopes []string
		wantSecret string
	}{
		{
			name:     "default provider is gitlab",
			cfg:      Config{},
			wantProv: AuthProviderGitLab,
		},
		{
			name: "unknown provider is rejected",
			cfg: Config{
				Auth: Auth{Provider: "ldap"},
			},
			wantErr: "not supported",
		},
		{
			name: "gitlab with only admin_users mismatch is rejected",
			cfg: Config{
				AdminUsers: []string{"alice"},
			},
			wantErr: "admin_gitlab_ids",
		},
		{
			name: "oidc requires issuer_url",
			cfg: Config{
				Auth: Auth{Provider: AuthProviderOIDC, OIDC: AuthOIDC{
					ClientID: "x", ClientSecret: "y", RedirectURL: "https://app/cb",
					AllowedGroups: []string{"g"},
				}},
			},
			wantErr: "issuer_url",
		},
		{
			name: "oidc requires allowed_groups",
			cfg: Config{
				Auth: Auth{Provider: AuthProviderOIDC, OIDC: AuthOIDC{
					IssuerURL: "https://idp", ClientID: "x", ClientSecret: "y",
					RedirectURL: "https://app/cb",
				}},
			},
			wantErr: "allowed_groups",
		},
		{
			name: "oidc requires client_secret",
			cfg: Config{
				Auth: Auth{Provider: AuthProviderOIDC, OIDC: AuthOIDC{
					IssuerURL: "https://idp", ClientID: "x",
					RedirectURL:   "https://app/cb",
					AllowedGroups: []string{"g"},
				}},
			},
			wantErr: "client_secret",
		},
		{
			name: "oidc resolves env_client_secret",
			cfg: Config{
				Auth: Auth{Provider: AuthProviderOIDC, OIDC: AuthOIDC{
					IssuerURL: "https://idp", ClientID: "x",
					EnvClientSecret: envSecret,
					RedirectURL:     "https://app/cb",
					AllowedGroups:   []string{"g"},
				}},
				AdminUsers: []string{"alice"},
			},
			env:        map[string]string{envSecret: "from-env"},
			wantProv:   AuthProviderOIDC,
			wantSecret: "from-env",
			wantScopes: []string{"openid", "profile", "email", "groups"},
		},
		{
			name: "oidc env_client_secret missing in env is rejected",
			cfg: Config{
				Auth: Auth{Provider: AuthProviderOIDC, OIDC: AuthOIDC{
					IssuerURL: "https://idp", ClientID: "x",
					EnvClientSecret: envSecret,
					RedirectURL:     "https://app/cb",
					AllowedGroups:   []string{"g"},
				}},
			},
			wantErr: envSecret,
		},
		{
			name: "oidc with only admin_gitlab_ids mismatch is rejected",
			cfg: Config{
				Auth:           Auth{Provider: AuthProviderOIDC, OIDC: validOIDC()},
				AdminGitLabIDs: []int64{42},
			},
			wantErr: "admin_users",
		},
		{
			name: "oidc happy path defaults scopes",
			cfg: Config{
				Auth:       Auth{Provider: AuthProviderOIDC, OIDC: validOIDC()},
				AdminUsers: []string{"alice"},
			},
			wantProv:   AuthProviderOIDC,
			wantScopes: []string{"openid", "profile", "email", "groups"},
			wantSecret: "the-secret",
		},
		{
			name: "oidc keeps user-provided scopes",
			cfg: func() Config {
				o := validOIDC()
				o.Scopes = []string{"openid", "groups"}
				return Config{
					Auth:       Auth{Provider: AuthProviderOIDC, OIDC: o},
					AdminUsers: []string{"alice"},
				}
			}(),
			wantProv:   AuthProviderOIDC,
			wantScopes: []string{"openid", "groups"},
			wantSecret: "the-secret",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			cfg := tt.cfg
			err := cfg.validateAuth()
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantProv != "" && cfg.Auth.Provider != tt.wantProv {
				t.Errorf("provider = %q, want %q", cfg.Auth.Provider, tt.wantProv)
			}
			if tt.wantScopes != nil && !slices.Equal(cfg.Auth.OIDC.Scopes, tt.wantScopes) {
				t.Errorf("scopes = %v, want %v", cfg.Auth.OIDC.Scopes, tt.wantScopes)
			}
			if tt.wantSecret != "" && cfg.Auth.OIDC.ClientSecret != tt.wantSecret {
				t.Errorf("client_secret = %q, want %q", cfg.Auth.OIDC.ClientSecret, tt.wantSecret)
			}
		})
	}
}

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
