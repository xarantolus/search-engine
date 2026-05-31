package web

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	gitlab "gitlab.com/gitlab-org/api/client-go"
	"golang.org/x/oauth2"
)

type UserInfo struct {
	GitLabUserID     int64
	DisplayName      string
	PermissionGroups []string
	IsAdmin          bool
}

func (s *Server) UserInfo(c *fiber.Ctx, idOverwrite ...int64) (ui UserInfo, err error) {
	var permissionGroups []string

	// Check if API key is present and valid
	if adm, perm, err := s.Config.PermissionsForToken(c.Query("apiKey")); err == nil {
		// Use API key to get permission groups
		permissionGroups = perm
		ui.PermissionGroups = permissionGroups
		ui.IsAdmin = adm
		ui.GitLabUserID = -1
		ui.DisplayName = "API User"
		return ui, nil
	}

	sess, err := s.Session.Get(c)
	if err != nil {
		return ui, err
	}

	var userid int64
	if len(idOverwrite) > 0 {
		userid = idOverwrite[0]
	} else {
		user_id, ok := sess.Get("gitlab_user_id").(int64)
		if !ok {
			return ui, fmt.Errorf("failed to get gitlab_user_id")
		}
		userid = user_id
	}

	userInfo, err := s.FiberStore.Get(fmt.Sprintf("user_info:%d", userid))
	if err != nil {
		return ui, fmt.Errorf("failed to get user_info:%d", userid)
	}

	err = json.Unmarshal(userInfo, &ui)
	if err != nil {
		return ui, fmt.Errorf("failed to unmarshal user_info:%d", userid)
	}

	ui.IsAdmin = slices.Contains(s.Config.AdminGitLabIDs, ui.GitLabUserID)

	// Check if user has permission groups
	if len(ui.PermissionGroups) == 0 {
		return ui, fmt.Errorf("You are not permitted to view any files. Ask an administrator for access.")
	}

	return
}

func (s *Server) AddLoggedInUser(c *fiber.Ctx, userInfo UserInfo) error {
	sess, err := s.Session.Get(c)
	if err != nil {
		return err
	}

	userInfoBytes, err := json.Marshal(userInfo)
	if err != nil {
		return err
	}

	err = s.FiberStore.Set(fmt.Sprintf("user_info:%d", userInfo.GitLabUserID), userInfoBytes, 100*365*24*time.Hour)
	if err != nil {
		return err
	}

	sess.Set("gitlab_user_id", userInfo.GitLabUserID)

	// Now also add it globally, so the admin UI knows who is logged in
	s.fiberStoreLock.Lock()
	defer s.fiberStoreLock.Unlock()

	var userids []int64
	bytes, err := s.FiberStore.Get("user_ids")
	if err != nil {
		log.Println("Error getting user_ids:", err)
	}
	err = json.Unmarshal(bytes, &userids)
	if err != nil {
		log.Println("Error unmarshalling user_ids:", err)
	}

	if !slices.Contains(userids, userInfo.GitLabUserID) {
		userids = append(userids, userInfo.GitLabUserID)
		bytes, err = json.Marshal(userids)
		if err != nil {
			return err
		}

		err = s.FiberStore.Set("user_ids", bytes, 100*365*24*time.Hour)
		if err != nil {
			return c.SendStatus(fiber.StatusInternalServerError)
		}
	}

	return sess.Save()
}

func (s *Server) LoginMiddleware(c *fiber.Ctx) error {
	if c.Path() == "/login" || c.Path() == "/callback" {
		return c.Next()
	}

	var reject = func() error {
		if strings.HasPrefix(c.Path(), "/api") {
			return c.SendStatus(fiber.StatusUnauthorized)
		}

		return c.Redirect(fmt.Sprintf("/login?redirect=%s", url.QueryEscape(c.Path())))
	}

	key := c.Query("apiKey")
	if key == "" {
		sess, err := s.Session.Get(c)
		if err != nil || sess.Get("gitlab_user_id") == nil {
			return reject()
		}
	} else {
		// Check API key
		_, _, err := s.Config.PermissionsForToken(key)
		if err != nil {
			return reject()
		}
	}

	if err := c.Next(); err != nil {
		return err
	}

	return nil
}

func (s *Server) LoginRoute(c *fiber.Ctx) error {
	// Get the redirect path from the query parameter
	redirectPath := c.Query("redirect", "")
	if redirectPath == "/" || redirectPath == "/login" || redirectPath == "/callback" {
		redirectPath = ""
	}

	// Generate the OAuth2 login URL with the state parameter
	state := url.QueryEscape(redirectPath)
	authURL := s.OauthConfig.AuthCodeURL(state, oauth2.AccessTypeOffline)

	// Store the state in the session for validation
	sess, err := s.Session.Get(c)
	if err != nil {
		log.Println("Error getting session:", err)
		return c.SendStatus(fiber.StatusInternalServerError)
	}
	sess.Set("oauth_state", state)
	if err := sess.Save(); err != nil {
		log.Println("Error saving session:", err)
		return c.SendStatus(fiber.StatusInternalServerError)
	}

	return c.Redirect(authURL)
}

func (s *Server) LoginCallback(c *fiber.Ctx) error {
	code := c.Query("code")
	if code == "" {
		return fmt.Errorf("no code in query")
	}

	// Validate the state parameter
	state := c.Query("state")
	sess, err := s.Session.Get(c)
	if err != nil {
		log.Println("Error getting session:", err)
		return c.SendStatus(fiber.StatusInternalServerError)
	}
	savedState := sess.Get("oauth_state")
	if savedState == nil || savedState != state {
		return c.SendStatus(fiber.StatusForbidden)
	}

	token, err := s.OauthConfig.Exchange(c.Context(), code)
	if err != nil {
		return fmt.Errorf("failed to exchange code: %w", err)
	}

	gitlabClient, err := gitlab.NewOAuthClient(token.AccessToken, gitlab.WithBaseURL(s.GitLab.GitLabBaseURL.String()))
	if err != nil {
		return fmt.Errorf("failed to create gitlab client: %w", err)
	}

	gitlabUser, _, err := gitlabClient.Users.CurrentUser(gitlab.WithContext(c.Context()))
	if err != nil {
		return fmt.Errorf("failed to get logged in gitlab user info: %w", err)
	}

	// also check inherited groups
	groupMember, _, err := gitlabClient.GroupMembers.GetInheritedGroupMember(strconv.FormatInt(s.GitLab.AllowedGitLabGroupID, 10), gitlabUser.ID, nil)
	if err != nil {
		return fmt.Errorf("failed to get group member info: %w", err)
	}

	if groupMember.AccessLevel == gitlab.NoPermissions {
		return c.SendStatus(fiber.StatusForbidden)
	}

	sess.Set("gitlab_user_id", int64(gitlabUser.ID))

	// See if we already have this user so we keep their permissions
	userInfo, err := s.UserInfo(c, int64(gitlabUser.ID))
	if err != nil {
		userInfo.GitLabUserID = int64(gitlabUser.ID)
		userInfo.PermissionGroups = s.Config.GetDefaultPermissionGroups()
	}
	userInfo.DisplayName = gitlabUser.Name

	err = s.AddLoggedInUser(c, userInfo)
	if err != nil {
		return fmt.Errorf("failed to add logged in user: %w", err)
	}
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

func isValidRedirectPath(path string) bool {
	// Parse the URL to check if it is absolute
	parsedURL, err := url.Parse(path)
	if err != nil {
		return false
	}

	// Ensure the URL is not absolute and does not contain suspicious characters
	if parsedURL.IsAbs() || strings.Contains(path, "//") || strings.Contains(path, "..") {
		return false
	}

	return true
}

func (s *Server) LogoutRoute(c *fiber.Ctx) error {
	sess, err := s.Session.Get(c)
	if err != nil {
		log.Println("Error getting session:", err)
		return c.SendStatus(fiber.StatusInternalServerError)
	}

	if err := sess.Destroy(); err != nil {
		return c.SendStatus(fiber.StatusInternalServerError)
	}

	return c.SendString("Logged out")
}
