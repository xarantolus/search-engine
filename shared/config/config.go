package config

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the configuration for the wiki scraper
type Config struct {
	AppName         string                 `yaml:"app_name"`
	IndexName       string                 `yaml:"index_name"`
	GitLogins       []GitLogins            `yaml:"git_logins"`
	Repositories    map[string]Repository  `yaml:"repositories"`
	GitlabGroups    map[string]GitlabGroup `yaml:"gitlab_groups"`
	GiteaOrgs       map[string]GiteaOrg    `yaml:"gitea_orgs"`
	GithubOrgs      map[string]GithubOrg   `yaml:"github_orgs"`
	Mounts          map[string]Mount       `yaml:"mounts"`
	Websites        map[string]Website     `yaml:"websites"`
	DefaultIncludes string                 `yaml:"default_includes"`
	DefaultExcludes string                 `yaml:"default_excludes"`
	Conflucence     map[string]Confluence  `yaml:"confluence"`
	Cache           Cache                  `yaml:"cache"`
	Synonyms        Synonyms               `yaml:"synonyms"`
	Dictionary      []string               `yaml:"dictionary"`

	APIKeys []APIKey `yaml:"api_keys"`

	PermissionGroups []PermissionGroup `yaml:"permission_groups"`
	AdminGitLabIDs   []int64           `yaml:"admin_gitlab_ids"`
	AdminUsers       []string          `yaml:"admin_users"`

	Auth Auth `yaml:"auth"`

	MaxDocumentSize    int           `yaml:"max_document_size"`
	ParallelJobs       int           `yaml:"parallel_jobs"`
	TextWorkers        int           `yaml:"text_workers"`
	DefaultJobInterval time.Duration `yaml:"default_interval"`
	MinCommit          int           `yaml:"min_commit"`
	DeleteAfterTime    time.Duration `yaml:"delete_after_time"`
}

// Auth selects and configures the login provider.
//
// Provider must be one of:
//   - "gitlab" (default; existing GitLab OAuth flow, configured via env vars
//     consumed by GitLabFromEnv)
//   - "oidc" (generic OpenID Connect, e.g. FreeIPA via Keycloak; configured
//     via the OIDC sub-block below)
type Auth struct {
	Provider string   `yaml:"provider"`
	OIDC     AuthOIDC `yaml:"oidc"`
}

const (
	AuthProviderGitLab = "gitlab"
	AuthProviderOIDC   = "oidc"
)

// AuthOIDC configures the generic OpenID Connect provider.
//
// AllowedGroups are matched against the "groups" claim of the ID token (or,
// failing that, the userinfo endpoint). The user must be a member of at least
// one. Group entries are plain names (e.g. "search-users"), not DNs.
//
// ClientSecret is read from EnvClientSecret if set, otherwise from the inline
// ClientSecret field. Prefer the env variant so secrets stay out of YAML.
type AuthOIDC struct {
	IssuerURL       string   `yaml:"issuer_url"`
	ClientID        string   `yaml:"client_id"`
	ClientSecret    string   `yaml:"client_secret"`
	EnvClientSecret string   `yaml:"env_client_secret"`
	RedirectURL     string   `yaml:"redirect_url"`
	AllowedGroups   []string `yaml:"allowed_groups"`
	Scopes          []string `yaml:"scopes"`
}

// validateAuth normalizes Auth defaults and rejects misconfigurations.
//
// It mutates c (defaulting Provider to "gitlab", filling in default OIDC
// scopes, resolving EnvClientSecret) and returns an error if the resulting
// config is unusable for the selected provider.
func (c *Config) validateAuth() error {
	if c.Auth.Provider == "" {
		c.Auth.Provider = AuthProviderGitLab
	}

	switch c.Auth.Provider {
	case AuthProviderGitLab:
		// GitLab-specific environment is read in main.go via GitLabFromEnv.
		// We still warn about the silent-misconfig case where only the
		// OIDC-side admin list is populated.
		if len(c.AdminGitLabIDs) == 0 && len(c.AdminUsers) > 0 {
			return fmt.Errorf("auth.provider %q uses admin_gitlab_ids, but only admin_users is set", c.Auth.Provider)
		}
	case AuthProviderOIDC:
		o := &c.Auth.OIDC
		if o.IssuerURL == "" {
			return fmt.Errorf("auth.oidc.issuer_url is required when auth.provider is %q", AuthProviderOIDC)
		}
		if _, err := url.ParseRequestURI(o.IssuerURL); err != nil {
			return fmt.Errorf("auth.oidc.issuer_url is not a valid URL: %w", err)
		}
		if o.ClientID == "" {
			return fmt.Errorf("auth.oidc.client_id is required when auth.provider is %q", AuthProviderOIDC)
		}
		if o.RedirectURL == "" {
			return fmt.Errorf("auth.oidc.redirect_url is required when auth.provider is %q", AuthProviderOIDC)
		}
		if _, err := url.ParseRequestURI(o.RedirectURL); err != nil {
			return fmt.Errorf("auth.oidc.redirect_url is not a valid URL: %w", err)
		}
		if len(o.AllowedGroups) == 0 {
			return fmt.Errorf("auth.oidc.allowed_groups must list at least one group when auth.provider is %q", AuthProviderOIDC)
		}
		if o.EnvClientSecret != "" {
			secret := os.Getenv(o.EnvClientSecret)
			if secret == "" {
				return fmt.Errorf("auth.oidc.env_client_secret %q is not set in the environment", o.EnvClientSecret)
			}
			o.ClientSecret = secret
		}
		if o.ClientSecret == "" {
			return fmt.Errorf("auth.oidc.client_secret (or env_client_secret) is required when auth.provider is %q", AuthProviderOIDC)
		}
		if len(o.Scopes) == 0 {
			o.Scopes = []string{"openid", "profile", "email", "groups"}
		}
		if len(c.AdminUsers) == 0 && len(c.AdminGitLabIDs) > 0 {
			return fmt.Errorf("auth.provider %q uses admin_users, but only admin_gitlab_ids is set", c.Auth.Provider)
		}
	default:
		return fmt.Errorf("auth.provider %q is not supported (use %q or %q)", c.Auth.Provider, AuthProviderGitLab, AuthProviderOIDC)
	}
	return nil
}

type APIKey struct {
	EnvKey           string `yaml:"env_key"`
	Token            string
	PermissionGroups []string `yaml:"permission_groups"`
	IsAdmin          bool     `yaml:"is_admin"`
}

type Website struct {
	URL string `yaml:"url"`

	Interval      time.Duration `yaml:"interval"`
	PermissionTag string        `yaml:"permission_tag"`
}

func (c *Config) PermissionsForToken(token string) (admin bool, groups []string, err error) {
	for _, key := range c.APIKeys {
		if subtle.ConstantTimeCompare([]byte(key.Token), []byte(token)) == 1 {
			if len(key.PermissionGroups) == 0 {
				panic(fmt.Sprintf("API key %q has no permission groups set", key.EnvKey))
			}
			return key.IsAdmin, key.PermissionGroups, nil
		}
	}
	return false, nil, errors.New("invalid API token")
}

func (c *Config) GetDefaultPermissionGroups() []string {
	var names []string
	for _, group := range c.PermissionGroups {
		if group.Default {
			names = append(names, group.Name)
		}
	}
	return names
}

// UserCanAccessTag returns true if any of the given permission groups grants access to tag.
func (c *Config) UserCanAccessTag(permissionGroups []string, tag string) bool {
	return slices.Contains(c.GetGroupsTags(permissionGroups), tag)
}

func (c *Config) GetGroupsTags(groupNames []string) []string {
	var tags []string
	for _, group := range c.PermissionGroups {
		for _, name := range groupNames {
			if group.Name == name {
				tags = append(tags, group.Tags...)
			}
		}
	}
	return tags
}

type PermissionGroup struct {
	Name    string `yaml:"name"`
	Default bool   `yaml:"default"`

	Tags []string `yaml:"tags"`
}

type Synonyms struct {
	Bidirectional SynonymsMap `yaml:"bidirectional"`
	OneWay        SynonymsMap `yaml:"oneway"`

	Merged SynonymsMap `yaml:"-"`
}

type Confluence struct {
	SpaceKey    string `yaml:"space_key"`
	PageBaseURL string `yaml:"page_base_url"`
	APIURL      string `yaml:"api_url"`
	Token       string
	EnvToken    string        `yaml:"env_token"`
	Interval    time.Duration `yaml:"interval"`

	PermissionTag string `yaml:"permission_tag"`
}

type GitLogins struct {
	Host  string `yaml:"host"`
	Token string

	EnvToken string `yaml:"env_token"`
}

type GitlabGroup struct {
	Host     string        `yaml:"host"`
	ID       string        `yaml:"id"`
	Interval time.Duration `yaml:"interval"`

	IndexRepos         bool          `yaml:"index_repos"`
	IndexReposInterval time.Duration `yaml:"index_repos_interval"`

	IndexReposIncludeGlob           string `yaml:"index_repos_include"`
	IndexReposExcludeGlob           string `yaml:"index_repos_exclude"`
	IndexReposPermissionTagOverride string `yaml:"index_repos_permission_tag"`

	IndexReposIgnoreIssuesPRs bool `yaml:"index_repos_ignore_issues_prs"`

	ScrapeIssues        bool `yaml:"scrape_issues"`
	ScrapeMergeRequests bool `yaml:"scrape_merge_requests"`

	PermissionTag string `yaml:"permission_tag"`
}

// GiteaOrg describes a Gitea organisation or user whose repositories should
// be auto-discovered and indexed. Mirrors GitlabGroup's shape.
type GiteaOrg struct {
	Host     string        `yaml:"host"`
	Owner    string        `yaml:"owner"`
	IsUser   bool          `yaml:"is_user"`
	Interval time.Duration `yaml:"interval"`

	IndexRepos         bool          `yaml:"index_repos"`
	IndexReposInterval time.Duration `yaml:"index_repos_interval"`

	IndexReposIncludeGlob           string `yaml:"index_repos_include"`
	IndexReposExcludeGlob           string `yaml:"index_repos_exclude"`
	IndexReposPermissionTagOverride string `yaml:"index_repos_permission_tag"`

	IndexReposIgnoreIssuesPRs bool `yaml:"index_repos_ignore_issues_prs"`

	ScrapeIssues       bool `yaml:"scrape_issues"`
	ScrapePullRequests bool `yaml:"scrape_pull_requests"`

	PermissionTag string `yaml:"permission_tag"`
}

// GithubOrg describes a GitHub organisation or user account. The GitHub API
// host is always github.com today, so no Host field is needed.
type GithubOrg struct {
	Owner    string        `yaml:"owner"`
	IsUser   bool          `yaml:"is_user"`
	Interval time.Duration `yaml:"interval"`

	IndexRepos         bool          `yaml:"index_repos"`
	IndexReposInterval time.Duration `yaml:"index_repos_interval"`

	IndexReposIncludeGlob           string `yaml:"index_repos_include"`
	IndexReposExcludeGlob           string `yaml:"index_repos_exclude"`
	IndexReposPermissionTagOverride string `yaml:"index_repos_permission_tag"`

	IndexReposIgnoreIssuesPRs bool `yaml:"index_repos_ignore_issues_prs"`

	ScrapeIssues       bool `yaml:"scrape_issues"`
	ScrapePullRequests bool `yaml:"scrape_pull_requests"`

	PermissionTag string `yaml:"permission_tag"`
}

type Repository struct {
	URL    string `yaml:"url"`
	IsWiki bool   `yaml:"wiki"`
	// Comma-separated globs, e.g. "*.md, *.txt".
	// If empty, default_includes will be used, and if that is empty, all files will be included.
	Include         string        `yaml:"include"`
	Exclude         string        `yaml:"exclude"`
	IgnoreIssuesPRs bool          `yaml:"ignore_issues_prs"`
	Interval        time.Duration `yaml:"interval"`

	PermissionTag string `yaml:"permission_tag"`
}

type Mount struct {
	NameOverride         string `yaml:"name"`
	Dir                  string `yaml:"dir"`
	Command              string `yaml:"command"`
	URLTransform         string `yaml:"url_transform"`
	InFolderURLTransform string `yaml:"in_folder_url_transform"`
	Include              string `yaml:"include"`
	Exclude              string `yaml:"exclude"`
	MaxFileSize          int64  `yaml:"max_size"`

	DisableCache bool `yaml:"disable_cache,omitempty"`

	Interval time.Duration `yaml:"interval"`

	PermissionTag string `yaml:"permission_tag"`

	// PathFilter defines a comma-separated, glob-based filter for paths (case-insensitive).
	// Only files where the relative path matches at least one of the globs will be indexed.
	PathFilter string `yaml:"path_filter,omitempty"`
}

type Cache struct {
	Dir     string `yaml:"dir"`
	TextDir string `yaml:"text_dir"`
	Repos   string `yaml:"repos"`
}

// UnmarshalYAML implements the yaml.Unmarshaler interface for Repository
func (r *Repository) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var url string
	if err := unmarshal(&url); err == nil {
		r.URL = url
		r.IsWiki = false
		return nil
	}

	var repo struct {
		URL             string `yaml:"url"`
		IsWiki          bool   `yaml:"wiki"`
		Include         string `yaml:"include"`
		IgnoreIssuesPRs bool   `yaml:"ignore_issues_prs"`
		Interval        time.Duration
		PermissionTag   string `yaml:"permission_tag"`
	}
	if err := unmarshal(&repo); err != nil {
		return err
	}

	r.URL = repo.URL
	r.IsWiki = repo.IsWiki
	r.Include = repo.Include
	r.IgnoreIssuesPRs = repo.IgnoreIssuesPRs
	r.Interval = repo.Interval
	r.PermissionTag = repo.PermissionTag
	return nil
}

func Parse(configPath string) (c Config, err error) {
	f, err := os.Open(configPath)
	if err != nil {
		return
	}
	defer f.Close()

	decoder := yaml.NewDecoder(f)
	decoder.KnownFields(true)
	err = decoder.Decode(&c)
	if err != nil {
		return
	}

	var (
		availablePermissionTags   = make(map[string]struct{})
		availablePermissionGroups = make(map[string]struct{})
		usedPermissionTags        = make(map[string]struct{})
	)
	for _, group := range c.PermissionGroups {
		for _, tag := range group.Tags {
			availablePermissionTags[tag] = struct{}{}
		}
		availablePermissionGroups[group.Name] = struct{}{}
	}

	for i := range c.GitLogins {
		token := os.Getenv(c.GitLogins[i].EnvToken)
		if token == "" {
			return Config{}, fmt.Errorf("git login %q requires %q env_token to be available in environment, but was not", c.GitLogins[i].Host, c.GitLogins[i].EnvToken)
		}

		c.GitLogins[i].Token = token
	}

	for i := range c.APIKeys {
		token := os.Getenv(c.APIKeys[i].EnvKey)
		if token == "" {
			return Config{}, fmt.Errorf("API key %q requires %q env_key to be available in environment, but was not", c.APIKeys[i].EnvKey, c.APIKeys[i].EnvKey)
		}
		if len(c.APIKeys[i].PermissionGroups) == 0 {
			return Config{}, fmt.Errorf("API key %q requires at least one permission group to be set", c.APIKeys[i].EnvKey)
		}
		for _, group := range c.APIKeys[i].PermissionGroups {
			if _, ok := availablePermissionGroups[group]; !ok {
				return Config{}, fmt.Errorf("API key %q has an permission group not used for any group: %q", c.APIKeys[i].EnvKey, group)
			}
			usedPermissionTags[group] = struct{}{}
		}
		c.APIKeys[i].Token = token
	}

	for name, group := range c.Websites {
		if group.URL == "" {
			return Config{}, fmt.Errorf("website %q has no URL", name)
		}
		if _, err := url.ParseRequestURI(group.URL); err != nil {
			return Config{}, fmt.Errorf("website %q has an invalid URL: %w", name, err)
		}

		if group.PermissionTag == "" {
			return Config{}, fmt.Errorf("website %q requires permission_tag to be set", name)
		}
		if _, ok := availablePermissionTags[group.PermissionTag]; !ok {
			return Config{}, fmt.Errorf("website %q has an permission tag not used for any group: %q", name, group.PermissionTag)
		}
		usedPermissionTags[group.PermissionTag] = struct{}{}
	}

	for i := range c.Conflucence {
		token := os.Getenv(c.Conflucence[i].EnvToken)
		if token == "" {
			return Config{}, fmt.Errorf("Confluence %q requires %q env_token to be available in environment, but was not", c.Conflucence[i].APIURL, c.Conflucence[i].EnvToken)
		}

		if c.Conflucence[i].SpaceKey == "" {
			return Config{}, fmt.Errorf("Confluence %q requires space_key to be set", c.Conflucence[i].APIURL)
		}

		if c.Conflucence[i].PageBaseURL == "" {
			return Config{}, fmt.Errorf("Confluence %q requires page_base_url to be set", c.Conflucence[i].APIURL)
		}

		if c.Conflucence[i].APIURL == "" {
			return Config{}, fmt.Errorf("Confluence %q requires api_url to be set", c.Conflucence[i].APIURL)
		}

		if c.Conflucence[i].PermissionTag == "" {
			return Config{}, fmt.Errorf("Confluence %q requires permission_tag to be set", c.Conflucence[i].APIURL)
		}

		c.Conflucence[i] = Confluence{
			SpaceKey:      c.Conflucence[i].SpaceKey,
			PageBaseURL:   c.Conflucence[i].PageBaseURL,
			APIURL:        c.Conflucence[i].APIURL,
			Token:         token,
			EnvToken:      c.Conflucence[i].EnvToken,
			Interval:      c.Conflucence[i].Interval,
			PermissionTag: c.Conflucence[i].PermissionTag,
		}

		if _, ok := availablePermissionTags[c.Conflucence[i].PermissionTag]; !ok {
			return Config{}, fmt.Errorf("Confluence %q has an permission tag not used for any group: %q", c.Conflucence[i].APIURL, c.Conflucence[i].PermissionTag)
		}
		usedPermissionTags[c.Conflucence[i].PermissionTag] = struct{}{}
	}

	for name, repo := range c.Repositories {
		if repo.URL == "" {
			return Config{}, fmt.Errorf("repository %q has no URL", name)
		}

		if repo.IsWiki && !repo.IgnoreIssuesPRs {
			return Config{}, fmt.Errorf("repository %q is a wiki, so it requires ignore_issues_prs to be false", name)
		}

		repo.Include = strings.ToLower(repo.Include)

		if repo.PermissionTag == "" {
			return Config{}, fmt.Errorf("repository %q requires permission_tag to be set", name)
		}
		if _, ok := availablePermissionTags[repo.PermissionTag]; !ok {
			return Config{}, fmt.Errorf("repository %q has an permission tag not used for any group: %q", name, repo.PermissionTag)
		}
		usedPermissionTags[repo.PermissionTag] = struct{}{}
	}

	for name, group := range c.GitlabGroups {
		if group.Host == "" {
			return Config{}, fmt.Errorf("gitlab group %q has no host", name)
		}

		if group.ID == "" {
			return Config{}, fmt.Errorf("gitlab group %q has no ID", name)
		}

		if group.PermissionTag == "" {
			return Config{}, fmt.Errorf("gitlab group %q requires permission_tag to be set", name)
		}

		if _, ok := availablePermissionTags[group.PermissionTag]; !ok {
			return Config{}, fmt.Errorf("gitlab group %q has an permission tag not used for any group: %q", name, group.PermissionTag)
		}
		usedPermissionTags[group.PermissionTag] = struct{}{}

		if group.IndexReposPermissionTagOverride != "" {
			if _, ok := availablePermissionTags[group.IndexReposPermissionTagOverride]; !ok {
				return Config{}, fmt.Errorf("gitlab group %q has an permission tag override not used for any group: %q", name, group.IndexReposPermissionTagOverride)
			}
			usedPermissionTags[group.IndexReposPermissionTagOverride] = struct{}{}
		}
	}

	for name, org := range c.GiteaOrgs {
		if org.Host == "" {
			return Config{}, fmt.Errorf("gitea org %q has no host", name)
		}
		if _, err := url.ParseRequestURI(org.Host); err != nil {
			return Config{}, fmt.Errorf("gitea org %q has an invalid host: %w", name, err)
		}
		if org.Owner == "" {
			return Config{}, fmt.Errorf("gitea org %q has no owner", name)
		}
		if org.PermissionTag == "" {
			return Config{}, fmt.Errorf("gitea org %q requires permission_tag to be set", name)
		}
		if _, ok := availablePermissionTags[org.PermissionTag]; !ok {
			return Config{}, fmt.Errorf("gitea org %q has an permission tag not used for any group: %q", name, org.PermissionTag)
		}
		usedPermissionTags[org.PermissionTag] = struct{}{}

		if org.IndexReposPermissionTagOverride != "" {
			if _, ok := availablePermissionTags[org.IndexReposPermissionTagOverride]; !ok {
				return Config{}, fmt.Errorf("gitea org %q has an permission tag override not used for any group: %q", name, org.IndexReposPermissionTagOverride)
			}
			usedPermissionTags[org.IndexReposPermissionTagOverride] = struct{}{}
		}
	}

	for name, org := range c.GithubOrgs {
		if org.Owner == "" {
			return Config{}, fmt.Errorf("github org %q has no owner", name)
		}
		if org.PermissionTag == "" {
			return Config{}, fmt.Errorf("github org %q requires permission_tag to be set", name)
		}
		if _, ok := availablePermissionTags[org.PermissionTag]; !ok {
			return Config{}, fmt.Errorf("github org %q has an permission tag not used for any group: %q", name, org.PermissionTag)
		}
		usedPermissionTags[org.PermissionTag] = struct{}{}

		if org.IndexReposPermissionTagOverride != "" {
			if _, ok := availablePermissionTags[org.IndexReposPermissionTagOverride]; !ok {
				return Config{}, fmt.Errorf("github org %q has an permission tag override not used for any group: %q", name, org.IndexReposPermissionTagOverride)
			}
			usedPermissionTags[org.IndexReposPermissionTagOverride] = struct{}{}
		}
	}

	for name, mount := range c.Mounts {
		if mount.Dir == "" {
			return Config{}, fmt.Errorf("mount %q has no dir", name)
		}

		if mount.PermissionTag == "" {
			return Config{}, fmt.Errorf("mount %q requires permission_tag to be set", name)
		}

		if mount.URLTransform == "" {
			return Config{}, fmt.Errorf("mount %q requires url_transform to be set", name)
		}

		if _, ok := availablePermissionTags[mount.PermissionTag]; !ok {
			return Config{}, fmt.Errorf("mount %q has an permission tag not used for any group: %q", name, mount.PermissionTag)
		}

		// make slugfilter case-insensitive
		if mount.PathFilter != "" {
			m := c.Mounts[name]
			m.PathFilter = strings.ToLower(mount.PathFilter)
			c.Mounts[name] = m
		}

		usedPermissionTags[mount.PermissionTag] = struct{}{}
	}

	// Find permission tags that are not used
	for tag := range availablePermissionTags {
		if _, ok := usedPermissionTags[tag]; !ok {
			return Config{}, fmt.Errorf("permission tag %q is not used by any indexing job", tag)
		}
	}

	if err := c.validateAuth(); err != nil {
		return Config{}, err
	}

	// Set up synonyms
	if c.Synonyms.Bidirectional == nil {
		c.Synonyms.Bidirectional = make(SynonymsMap)
	}
	c.Synonyms.Bidirectional = c.Synonyms.Bidirectional.Bidirectional()
	if c.Synonyms.OneWay == nil {
		c.Synonyms.OneWay = make(SynonymsMap)
	}
	c.Synonyms.Merged = c.Synonyms.Bidirectional.Merge(c.Synonyms.OneWay)

	if c.ParallelJobs <= 0 {
		c.ParallelJobs = runtime.NumCPU() / 2
		if c.ParallelJobs < 1 {
			c.ParallelJobs = 1
		}
		fmt.Printf("[Config]: setting parallel jobs to %d\n", c.ParallelJobs)
	}

	if c.TextWorkers <= 0 {
		c.TextWorkers = 1
		fmt.Printf("[Config]: setting number of text workers per job to %d\n", c.TextWorkers)
	}

	if c.DefaultJobInterval <= 0 {
		c.DefaultJobInterval = 4 * time.Hour
		fmt.Printf("[Config]: setting job interval to %s\n", c.DefaultJobInterval)
	}

	if c.MinCommit <= 0 {
		c.MinCommit = 25
		fmt.Printf("[Config]: setting min commit count to %d\n", c.MinCommit)
	}

	if c.DeleteAfterTime <= 0 {
		c.DeleteAfterTime = 14 * 24 * time.Hour
		fmt.Printf("[Config]: setting delete after time to %s\n", c.DeleteAfterTime)
	}

	err = os.MkdirAll(c.Cache.Dir, 0755)
	if err != nil {
		return Config{}, fmt.Errorf("failed to create cache directory: %w", err)
	}
	err = os.MkdirAll(c.Cache.TextDir, 0755)
	if err != nil {
		return Config{}, fmt.Errorf("failed to create text cache directory: %w", err)
	}

	slices.Sort(c.Dictionary)

	return
}

type GitLabInfo struct {
	HostExternalURL         *url.URL
	GitLabBaseURL           *url.URL
	GitLabApplicationID     string
	GitLabApplicationSecret string
	AllowedGitLabGroupID    int64
}

func GitLabFromEnv() (c GitLabInfo, err error) {
	var stringFromEnv = func(key string, target *string) error {
		val, ok := os.LookupEnv(key)
		if !ok {
			return fmt.Errorf("%s not set", key)
		}

		*target = val
		return nil
	}
	var urlFromEnv = func(key string, target **url.URL) error {
		val, err := url.Parse(os.Getenv(key))
		if err != nil {
			return fmt.Errorf("%s is not an URL: %w", key, err)
		}

		*target = val
		return nil
	}

	var intFromEnv = func(key string, target *int64, required bool) error {
		valStr, ok := os.LookupEnv(key)
		if !ok {
			if !required {
				return nil
			}
			return fmt.Errorf("%s not set", key)
		}

		val, err := strconv.ParseInt(valStr, 10, 64)
		if err != nil {
			return fmt.Errorf("%s is not an integer: %w", key, err)
		}

		*target = val
		return nil
	}

	if err = urlFromEnv("HOST_EXTERNAL_URL", &c.HostExternalURL); err != nil {
		return
	}

	if err = urlFromEnv("GITLAB_INSTANCE_URL", &c.GitLabBaseURL); err != nil {
		return
	}
	if err = stringFromEnv("GITLAB_APPLICATION_ID", &c.GitLabApplicationID); err != nil {
		return
	}
	if err = stringFromEnv("GITLAB_APPLICATION_SECRET", &c.GitLabApplicationSecret); err != nil {
		return
	}

	if err = intFromEnv("ALLOWED_GITLAB_GROUP_ID", &c.AllowedGitLabGroupID, true); err != nil {
		return
	}

	return
}

type SynonymsMap map[string][]string

func (s *SynonymsMap) UnmarshalYAML(unmarshal func(interface{}) error) error {
	// Basically, single elements can just be strings in the yaml
	var m map[string]interface{}
	if err := unmarshal(&m); err != nil {
		return err
	}

	*s = make(SynonymsMap)

	for k, v := range m {
		k = strings.ToLower(k)
		switch v := v.(type) {
		case string:
			(*s)[k] = []string{strings.ToLower(v)}
		case []interface{}:
			for _, v := range v {
				(*s)[k] = append((*s)[k], strings.ToLower(v.(string)))
			}
		default:
			return fmt.Errorf("expected string or list of strings, got %T", v)
		}
	}

	return nil
}

func (s SynonymsMap) Bidirectional() SynonymsMap {
	// Build an undirected adjacency list and collect all nodes
	adj := make(map[string][]string)
	nodes := make(map[string]struct{})
	for k, vs := range s {
		nodes[k] = struct{}{}
		for _, v := range vs {
			nodes[v] = struct{}{}
			adj[k] = append(adj[k], v)
			adj[v] = append(adj[v], k)
		}
	}

	result := make(SynonymsMap)
	visitedGlobal := make(map[string]bool)

	// For each unvisited node, BFS to find its connected component
	for start := range nodes {
		if visitedGlobal[start] {
			continue
		}
		queue := []string{start}
		visited := map[string]bool{start: true}
		component := []string{start}

		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			for _, nbr := range adj[cur] {
				if !visited[nbr] {
					visited[nbr] = true
					queue = append(queue, nbr)
					component = append(component, nbr)
				}
			}
		}

		// mark component nodes as globally visited
		for _, n := range component {
			visitedGlobal[n] = true
		}
		// every node in the component is synonyms with all others
		for _, n := range component {
			for _, m := range component {
				if m != n {
					result[n] = append(result[n], m)
				}
			}
		}
	}

	return result
}

// Helper function to append an item to a slice if it doesn't already exist
func appendIfMissing(slice []string, item string) []string {
	for _, v := range slice {
		if v == item {
			return slice
		}
	}
	return append(slice, item)
}

func (s SynonymsMap) Merge(other SynonymsMap) SynonymsMap {
	merged := make(SynonymsMap)

	// Add all entries from the current map
	for k, v := range s {
		merged[k] = append([]string{}, v...)
	}

	// Add all entries from the other map
	for k, v := range other {
		if _, exists := merged[k]; !exists {
			merged[k] = []string{}
		}
		merged[k] = append(merged[k], v...)
	}

	return merged
}
