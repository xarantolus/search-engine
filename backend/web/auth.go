package web

import (
	"github.com/gofiber/fiber/v2"
)

// UserInfo is the provider-agnostic identity returned to consumers.
//
// UserKey is opaque, owned by the auth provider. It must be stable across
// logins for the same human (e.g. "gitlab:12345" or "oidc:alice") so that
// admin permission edits persist across sessions.
type UserInfo struct {
	UserKey          string
	DisplayName      string
	PermissionGroups []string
	IsAdmin          bool
}

// AuthProvider is the contract every login backend implements.
//
// Implementations own their session-key naming, their storage layout and
// their notion of "admin": LoginRoute / LoginCallback / IsLoggedIn must
// agree on what a logged-in session looks like, and SaveUser persists the
// authoritative UserInfo for that key.
//
// ListUserKeys is used by the admin UI to enumerate every user the system
// has ever seen. Implementations are therefore expected to persist seen
// users locally; a stateless provider would return an empty list and the
// admin UI would be blank.
type AuthProvider interface {
	LoginRoute(c *fiber.Ctx) error
	LoginCallback(c *fiber.Ctx) error
	IsLoggedIn(c *fiber.Ctx) bool
	UserInfo(c *fiber.Ctx, keyOverride ...string) (UserInfo, error)
	SaveUser(c *fiber.Ctx, info UserInfo) error
	ListUserKeys() ([]string, error)
}
