package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"shared/config"
	"slices"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/session"
	"github.com/gofiber/storage/sqlite3"
	"golang.org/x/oauth2"
)

const (
	oidcSessionUserKey      = "user_key"
	oidcSessionState        = "oidc_state"
	oidcSessionNonce        = "oidc_nonce"
	oidcSessionPKCEVerifier = "oidc_pkce_verifier"
	oidcUserKeyPrefix       = "oidc:"
)

// oidcAuth implements AuthProvider against any OpenID Connect IdP. It is
// used for FreeIPA (via its bundled / federated Keycloak) but the transport
// is generic.
//
// The login flow uses PKCE (S256) plus a `state` and a `nonce`. The ID
// token is verified against the IdP's JWKS (signature, iss, aud, exp, nbf,
// nonce). Group membership is read from the `groups` claim of the ID
// token; if that claim is absent, the userinfo endpoint is consulted as a
// fallback. The user must be in at least one of cfg.Auth.OIDC.AllowedGroups
// or they receive a 403, matching the GitLab provider's behaviour.
type oidcAuth struct {
	cfg         *config.Config
	provider    *oidc.Provider
	verifier    *oidc.IDTokenVerifier
	oauthConfig *oauth2.Config
	session     *session.Store
	store       *sqlite3.Storage
	users       *UserIndex
}

// NewOIDCAuth performs OIDC discovery against cfg.Auth.OIDC.IssuerURL and
// returns a ready-to-use provider. It blocks on the discovery HTTP call.
func NewOIDCAuth(
	ctx context.Context,
	cfg *config.Config,
	sessionStore *session.Store,
	store *sqlite3.Storage,
	users *UserIndex,
) (AuthProvider, error) {
	o := cfg.Auth.OIDC
	provider, err := oidc.NewProvider(ctx, o.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("oidc: discovery failed for %q: %w", o.IssuerURL, err)
	}
	verifier := provider.Verifier(&oidc.Config{ClientID: o.ClientID})
	return &oidcAuth{
		cfg:      cfg,
		provider: provider,
		verifier: verifier,
		oauthConfig: &oauth2.Config{
			ClientID:     o.ClientID,
			ClientSecret: o.ClientSecret,
			RedirectURL:  o.RedirectURL,
			Endpoint:     provider.Endpoint(),
			Scopes:       o.Scopes,
		},
		session: sessionStore,
		store:   store,
		users:   users,
	}, nil
}

func oidcUserKey(preferredUsername string) string {
	return oidcUserKeyPrefix + preferredUsername
}

func (a *oidcAuth) IsLoggedIn(c *fiber.Ctx) bool {
	sess, err := a.session.Get(c)
	if err != nil {
		return false
	}
	key, _ := sess.Get(oidcSessionUserKey).(string)
	return key != ""
}

func (a *oidcAuth) LoginRoute(c *fiber.Ctx) error {
	redirectPath := c.Query("redirect", "")
	if redirectPath == "/" || redirectPath == "/login" || redirectPath == "/callback" {
		redirectPath = ""
	}
	state := url.QueryEscape(redirectPath) + "." + oauth2.GenerateVerifier()
	nonce := oauth2.GenerateVerifier()
	verifier := oauth2.GenerateVerifier()

	authURL := a.oauthConfig.AuthCodeURL(
		state,
		oidc.Nonce(nonce),
		oauth2.S256ChallengeOption(verifier),
	)

	sess, err := a.session.Get(c)
	if err != nil {
		log.Println("Error getting session:", err)
		return c.SendStatus(fiber.StatusInternalServerError)
	}
	sess.Set(oidcSessionState, state)
	sess.Set(oidcSessionNonce, nonce)
	sess.Set(oidcSessionPKCEVerifier, verifier)
	if err := sess.Save(); err != nil {
		log.Println("Error saving session:", err)
		return c.SendStatus(fiber.StatusInternalServerError)
	}
	return c.Redirect(authURL)
}

func (a *oidcAuth) LoginCallback(c *fiber.Ctx) error {
	code := c.Query("code")
	if code == "" {
		return fmt.Errorf("no code in query")
	}

	sess, err := a.session.Get(c)
	if err != nil {
		log.Println("Error getting session:", err)
		return c.SendStatus(fiber.StatusInternalServerError)
	}

	state := c.Query("state")
	savedState, _ := sess.Get(oidcSessionState).(string)
	if savedState == "" || savedState != state {
		return c.SendStatus(fiber.StatusForbidden)
	}
	expectedNonce, _ := sess.Get(oidcSessionNonce).(string)
	if expectedNonce == "" {
		return c.SendStatus(fiber.StatusForbidden)
	}
	verifier, _ := sess.Get(oidcSessionPKCEVerifier).(string)
	if verifier == "" {
		return c.SendStatus(fiber.StatusForbidden)
	}

	ctx := c.Context()
	token, err := a.oauthConfig.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return fmt.Errorf("oidc: failed to exchange code: %w", err)
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return fmt.Errorf("oidc: token response missing id_token")
	}
	idToken, err := a.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return fmt.Errorf("oidc: id_token verification failed: %w", err)
	}
	if idToken.Nonce != expectedNonce {
		return c.SendStatus(fiber.StatusForbidden)
	}

	var claims struct {
		PreferredUsername string   `json:"preferred_username"`
		Email             string   `json:"email"`
		Name              string   `json:"name"`
		Groups            []string `json:"groups"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return fmt.Errorf("oidc: failed to parse id_token claims: %w", err)
	}

	groups := claims.Groups
	if len(groups) == 0 {
		// Some IdPs (or some Keycloak setups without the Group Membership
		// mapper) omit `groups` from the ID token but expose it via the
		// userinfo endpoint. Try that before giving up.
		ui, err := a.provider.UserInfo(ctx, oauth2.StaticTokenSource(token))
		if err == nil {
			var uiClaims struct {
				PreferredUsername string   `json:"preferred_username"`
				Email             string   `json:"email"`
				Name              string   `json:"name"`
				Groups            []string `json:"groups"`
			}
			if err := ui.Claims(&uiClaims); err == nil {
				groups = uiClaims.Groups
				if claims.PreferredUsername == "" {
					claims.PreferredUsername = uiClaims.PreferredUsername
				}
				if claims.Email == "" {
					claims.Email = uiClaims.Email
				}
				if claims.Name == "" {
					claims.Name = uiClaims.Name
				}
			}
		}
	}

	if claims.PreferredUsername == "" {
		return fmt.Errorf("oidc: id_token has no preferred_username claim")
	}

	if !anyOverlap(groups, a.cfg.Auth.OIDC.AllowedGroups) {
		return c.SendStatus(fiber.StatusForbidden)
	}

	displayName := claims.Name
	if displayName == "" {
		displayName = claims.PreferredUsername
	}

	userKey := oidcUserKey(claims.PreferredUsername)
	prior, _ := a.loadUser(userKey)
	prior.UserKey = userKey
	prior.DisplayName = displayName
	if len(prior.PermissionGroups) == 0 {
		prior.PermissionGroups = a.cfg.GetDefaultPermissionGroups()
	}
	prior.IsAdmin = slices.Contains(a.cfg.AdminUsers, claims.PreferredUsername)

	if err := a.SaveUser(c, prior); err != nil {
		return fmt.Errorf("oidc: failed to save user: %w", err)
	}

	sess.Set(oidcSessionUserKey, userKey)
	if err := sess.Save(); err != nil {
		log.Println("Error saving session:", err)
		return c.SendStatus(fiber.StatusInternalServerError)
	}

	// state encodes "<originalRedirect>.<csrf>" — peel off the CSRF half.
	originalRedirect := state
	if idx := lastDot(originalRedirect); idx >= 0 {
		originalRedirect = originalRedirect[:idx]
	}
	originalRedirect, _ = url.QueryUnescape(originalRedirect)
	if originalRedirect == "" || !isValidRedirectPath(originalRedirect) {
		originalRedirect = "/"
	}
	return c.Redirect(originalRedirect)
}

func (a *oidcAuth) UserInfo(c *fiber.Ctx, keyOverride ...string) (UserInfo, error) {
	var userKey string
	if len(keyOverride) > 0 {
		userKey = keyOverride[0]
	} else {
		sess, err := a.session.Get(c)
		if err != nil {
			return UserInfo{}, err
		}
		k, _ := sess.Get(oidcSessionUserKey).(string)
		if k == "" {
			return UserInfo{}, fmt.Errorf("no user key in session")
		}
		userKey = k
	}

	info, err := a.loadUser(userKey)
	if err != nil {
		return UserInfo{}, err
	}
	if username, ok := oidcUsernameFromKey(userKey); ok {
		info.IsAdmin = slices.Contains(a.cfg.AdminUsers, username)
	}
	return info, nil
}

func (a *oidcAuth) SaveUser(c *fiber.Ctx, info UserInfo) error {
	bytes, err := json.Marshal(info)
	if err != nil {
		return err
	}
	if err := a.store.Set(userInfoStoreKey(info.UserKey), bytes, userInfoTTL); err != nil {
		return err
	}
	return a.users.Add(info.UserKey)
}

func (a *oidcAuth) ListUserKeys() ([]string, error) {
	return a.users.List()
}

func (a *oidcAuth) loadUser(userKey string) (UserInfo, error) {
	bytes, err := a.store.Get(userInfoStoreKey(userKey))
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

func oidcUsernameFromKey(userKey string) (string, bool) {
	if len(userKey) <= len(oidcUserKeyPrefix) || userKey[:len(oidcUserKeyPrefix)] != oidcUserKeyPrefix {
		return "", false
	}
	return userKey[len(oidcUserKeyPrefix):], true
}

func anyOverlap(have, allowed []string) bool {
	if len(have) == 0 || len(allowed) == 0 {
		return false
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, g := range allowed {
		allowedSet[g] = struct{}{}
	}
	for _, g := range have {
		if _, ok := allowedSet[g]; ok {
			return true
		}
	}
	return false
}

func lastDot(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '.' {
			return i
		}
	}
	return -1
}

var _ AuthProvider = (*oidcAuth)(nil)
