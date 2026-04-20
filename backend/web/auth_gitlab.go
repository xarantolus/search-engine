package web

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"shared/config"
	"slices"
	"strconv"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/session"
	"github.com/gofiber/storage/sqlite3"
	gitlab "gitlab.com/gitlab-org/api/client-go"
	"golang.org/x/oauth2"
)

const (
	gitlabSessionUserKey      = "user_key"
	gitlabSessionOAuthState   = "oauth_state"
	gitlabSessionPKCEVerifier = "pkce_verifier"
	gitlabUserKeyPrefix       = "gitlab:"
)

// gitlabAuth implements AuthProvider against a GitLab OAuth instance.
//
// User access is gated on membership in a single configured GitLab group;
// the `groups` semantics live entirely inside GitLab. PKCE (S256) is used
// even though GitLab also accepts plain code-flow — there's no reason not
// to.
type gitlabAuth struct {
	cfg         *config.Config
	info        config.GitLabInfo
	oauthConfig *oauth2.Config
	session     *session.Store
	store       *sqlite3.Storage
	users       *UserIndex
}

func NewGitLabAuth(
	cfg *config.Config,
	info config.GitLabInfo,
	sessionStore *session.Store,
	store *sqlite3.Storage,
	users *UserIndex,
) AuthProvider {
	resolve := func(base *url.URL, path string) string {
		return base.ResolveReference(&url.URL{Path: path}).String()
	}
	return &gitlabAuth{
		cfg:  cfg,
		info: info,
		oauthConfig: &oauth2.Config{
			ClientID:     info.GitLabApplicationID,
			ClientSecret: info.GitLabApplicationSecret,
			RedirectURL:  resolve(info.HostExternalURL, "/callback"),
			Scopes:       []string{"read_user", "openid", "read_api"},
			Endpoint: oauth2.Endpoint{
				AuthURL:       resolve(info.GitLabBaseURL, "/oauth/authorize"),
				TokenURL:      resolve(info.GitLabBaseURL, "/oauth/token"),
				DeviceAuthURL: resolve(info.GitLabBaseURL, "/oauth/device/code"),
			},
		},
		session: sessionStore,
		store:   store,
		users:   users,
	}
}

func gitlabUserKey(id int64) string {
	return fmt.Sprintf("%s%d", gitlabUserKeyPrefix, id)
}

func (g *gitlabAuth) IsLoggedIn(c *fiber.Ctx) bool {
	sess, err := g.session.Get(c)
	if err != nil {
		return false
	}
	key, _ := sess.Get(gitlabSessionUserKey).(string)
	return key != ""
}

func (g *gitlabAuth) LoginRoute(c *fiber.Ctx) error {
	redirectPath := c.Query("redirect", "")
	if redirectPath == "/" || redirectPath == "/login" || redirectPath == "/callback" {
		redirectPath = ""
	}
	state := url.QueryEscape(redirectPath)

	verifier := oauth2.GenerateVerifier()
	authURL := g.oauthConfig.AuthCodeURL(
		state,
		oauth2.AccessTypeOffline,
		oauth2.S256ChallengeOption(verifier),
	)

	sess, err := g.session.Get(c)
	if err != nil {
		log.Println("Error getting session:", err)
		return c.SendStatus(fiber.StatusInternalServerError)
	}
	sess.Set(gitlabSessionOAuthState, state)
	sess.Set(gitlabSessionPKCEVerifier, verifier)
	if err := sess.Save(); err != nil {
		log.Println("Error saving session:", err)
		return c.SendStatus(fiber.StatusInternalServerError)
	}
	return c.Redirect(authURL)
}

func (g *gitlabAuth) LoginCallback(c *fiber.Ctx) error {
	code := c.Query("code")
	if code == "" {
		return fmt.Errorf("no code in query")
	}

	sess, err := g.session.Get(c)
	if err != nil {
		log.Println("Error getting session:", err)
		return c.SendStatus(fiber.StatusInternalServerError)
	}

	state := c.Query("state")
	rawSavedState := sess.Get(gitlabSessionOAuthState)
	if rawSavedState == nil {
		return c.SendStatus(fiber.StatusForbidden)
	}
	savedState, ok := rawSavedState.(string)
	if !ok || savedState != state {
		return c.SendStatus(fiber.StatusForbidden)
	}
	verifier, _ := sess.Get(gitlabSessionPKCEVerifier).(string)
	if verifier == "" {
		return c.SendStatus(fiber.StatusForbidden)
	}

	token, err := g.oauthConfig.Exchange(c.Context(), code, oauth2.VerifierOption(verifier))
	if err != nil {
		return fmt.Errorf("failed to exchange code: %w", err)
	}

	gitlabClient, err := gitlab.NewOAuthClient(token.AccessToken, gitlab.WithBaseURL(g.info.GitLabBaseURL.String()))
	if err != nil {
		return fmt.Errorf("failed to create gitlab client: %w", err)
	}
	gitlabUser, _, err := gitlabClient.Users.CurrentUser(gitlab.WithContext(c.Context()))
	if err != nil {
		return fmt.Errorf("failed to get logged in gitlab user info: %w", err)
	}

	groupMember, _, err := gitlabClient.GroupMembers.GetGroupMember(strconv.FormatInt(g.info.AllowedGitLabGroupID, 10), gitlabUser.ID, nil)
	if err != nil {
		return fmt.Errorf("failed to get group member info: %w", err)
	}
	if groupMember.AccessLevel == gitlab.NoPermissions {
		return c.SendStatus(fiber.StatusForbidden)
	}

	userKey := gitlabUserKey(int64(gitlabUser.ID))

	prior, _ := g.loadUser(userKey)
	prior.UserKey = userKey
	prior.DisplayName = gitlabUser.Name
	if len(prior.PermissionGroups) == 0 {
		prior.PermissionGroups = g.cfg.GetDefaultPermissionGroups()
	}
	prior.IsAdmin = slices.Contains(g.cfg.AdminGitLabIDs, int64(gitlabUser.ID))

	if err := g.SaveUser(c, prior); err != nil {
		return fmt.Errorf("failed to save user: %w", err)
	}

	sess.Set(gitlabSessionUserKey, userKey)
	if err := sess.Save(); err != nil {
		log.Println("Error saving session:", err)
		return c.SendStatus(fiber.StatusInternalServerError)
	}

	state, _ = url.QueryUnescape(state)
	if state == "" || !isValidRedirectPath(state) {
		state = "/"
	}
	return c.Redirect(state)
}

func (g *gitlabAuth) UserInfo(c *fiber.Ctx, keyOverride ...string) (UserInfo, error) {
	var userKey string
	if len(keyOverride) > 0 {
		userKey = keyOverride[0]
	} else {
		sess, err := g.session.Get(c)
		if err != nil {
			return UserInfo{}, err
		}
		k, _ := sess.Get(gitlabSessionUserKey).(string)
		if k == "" {
			return UserInfo{}, fmt.Errorf("no user key in session")
		}
		userKey = k
	}

	info, err := g.loadUser(userKey)
	if err != nil {
		return UserInfo{}, err
	}
	if id, ok := gitlabIDFromKey(userKey); ok {
		info.IsAdmin = slices.Contains(g.cfg.AdminGitLabIDs, id)
	}
	return info, nil
}

func (g *gitlabAuth) SaveUser(c *fiber.Ctx, info UserInfo) error {
	bytes, err := json.Marshal(info)
	if err != nil {
		return err
	}
	if err := g.store.Set(userInfoStoreKey(info.UserKey), bytes, userInfoTTL); err != nil {
		return err
	}
	return g.users.Add(info.UserKey)
}

func (g *gitlabAuth) ListUserKeys() ([]string, error) {
	return g.users.List()
}

func (g *gitlabAuth) loadUser(userKey string) (UserInfo, error) {
	bytes, err := g.store.Get(userInfoStoreKey(userKey))
	if err != nil {
		return UserInfo{UserKey: userKey}, fmt.Errorf("failed to get %s: %w", userInfoStoreKey(userKey), err)
	}
	if len(bytes) == 0 {
		return UserInfo{UserKey: userKey}, fmt.Errorf("no record for %s", userInfoStoreKey(userKey))
	}
	var info UserInfo
	if err := json.Unmarshal(bytes, &info); err != nil {
		return UserInfo{UserKey: userKey}, fmt.Errorf("failed to unmarshal %s: %w", userInfoStoreKey(userKey), err)
	}
	info.UserKey = userKey
	return info, nil
}

func gitlabIDFromKey(userKey string) (int64, bool) {
	if len(userKey) <= len(gitlabUserKeyPrefix) || userKey[:len(gitlabUserKeyPrefix)] != gitlabUserKeyPrefix {
		return 0, false
	}
	id, err := strconv.ParseInt(userKey[len(gitlabUserKeyPrefix):], 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

var _ AuthProvider = (*gitlabAuth)(nil)
