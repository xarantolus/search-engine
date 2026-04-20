package web

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/storage/sqlite3"
)

const (
	// userInfoTTL is how long a per-user record lives in the FiberStore. The
	// session cookie itself expires after 28 days; one year here is just so
	// admin permission edits survive a user being away for a while without
	// keeping forever-stale records around.
	userInfoTTL = 365 * 24 * time.Hour

	storeKeyUserKeys      = "user_keys"
	storeKeyUserInfoFmt   = "user:%s"
	localsKeyUserInfoCtx  = "user_info"
	apiKeyQueryParam      = "apiKey"
	apiKeyUserKey         = "apikey"
	apiKeyUserDisplayName = "API User"
)

// UserIndex is a thin wrapper around the {seen-user-key -> []string} JSON
// blob in the FiberStore. The previous implementation inlined the
// read/append/JSON-marshal dance at every call site under a struct-level
// mutex; this consolidates it. Auth providers persist seen users through it
// so the admin UI can enumerate them.
type UserIndex struct {
	store *sqlite3.Storage
	mu    sync.Mutex
}

func NewUserIndex(store *sqlite3.Storage) *UserIndex {
	return &UserIndex{store: store}
}

// Add records key as a seen user. It is a no-op if the key is already known.
func (u *UserIndex) Add(key string) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	keys, err := u.listLocked()
	if err != nil {
		return err
	}
	for _, k := range keys {
		if k == key {
			return nil
		}
	}
	keys = append(keys, key)
	bytes, err := json.Marshal(keys)
	if err != nil {
		return err
	}
	return u.store.Set(storeKeyUserKeys, bytes, userInfoTTL)
}

// List returns every user key that has ever been recorded by Add. Order is
// insertion order.
func (u *UserIndex) List() ([]string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.listLocked()
}

func (u *UserIndex) listLocked() ([]string, error) {
	bytes, err := u.store.Get(storeKeyUserKeys)
	if err != nil {
		return nil, err
	}
	if len(bytes) == 0 {
		return nil, nil
	}
	var keys []string
	if err := json.Unmarshal(bytes, &keys); err != nil {
		// Stale / pre-migration data. Don't propagate; let the next Add
		// overwrite. (Same forgiving behaviour the old code had.)
		log.Printf("user_keys: ignoring unreadable index: %v", err)
		return nil, nil
	}
	return keys, nil
}

// userInfoStoreKey returns the FiberStore key under which a UserInfo blob
// is persisted for the given UserKey.
func userInfoStoreKey(userKey string) string {
	return fmt.Sprintf(storeKeyUserInfoFmt, userKey)
}

// UserInfo returns the identity for the current request.
//
// The lookup priority is:
//  1. cached UserInfo on c.Locals (avoids double FiberStore hits when the
//     middleware and the handler both ask)
//  2. API key short-circuit: a valid ?apiKey= grants the permission groups
//     and admin flag bound to that key, with a synthetic UserKey
//  3. delegation to the configured AuthProvider, which knows its own
//     session shape
//
// keyOverride bypasses the cache and the API-key short-circuit; it's used by
// the admin UI to look up other users.
func (s *Server) UserInfo(c *fiber.Ctx, keyOverride ...string) (UserInfo, error) {
	if len(keyOverride) == 0 {
		if cached, ok := c.Locals(localsKeyUserInfoCtx).(UserInfo); ok {
			return cached, nil
		}
		if adm, perm, err := s.Config.PermissionsForToken(c.Query(apiKeyQueryParam)); err == nil {
			info := UserInfo{
				UserKey:          apiKeyUserKey,
				DisplayName:      apiKeyUserDisplayName,
				PermissionGroups: perm,
				IsAdmin:          adm,
			}
			c.Locals(localsKeyUserInfoCtx, info)
			return info, nil
		}
	}

	info, err := s.Auth.UserInfo(c, keyOverride...)
	if err != nil {
		return info, err
	}
	if len(info.PermissionGroups) == 0 {
		return info, fmt.Errorf("You are not permitted to view any files. Ask an administrator for access.")
	}
	if len(keyOverride) == 0 {
		c.Locals(localsKeyUserInfoCtx, info)
	}
	return info, nil
}

// LoginMiddleware gates every route except the login flow itself (which is
// registered on the router before this middleware, so requests to /login or
// /callback never reach here).
func (s *Server) LoginMiddleware(c *fiber.Ctx) error {
	reject := func() error {
		if strings.HasPrefix(c.Path(), "/api") {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "not logged in"})
		}
		return c.Redirect(fmt.Sprintf("/login?redirect=%s", url.QueryEscape(c.Path())))
	}

	if key := c.Query(apiKeyQueryParam); key != "" {
		if _, _, err := s.Config.PermissionsForToken(key); err == nil {
			return c.Next()
		}
		return reject()
	}

	if !s.Auth.IsLoggedIn(c) {
		return reject()
	}
	return c.Next()
}

// LogoutRoute clears the session cookie. Per-user records in FiberStore are
// left in place so admin permission edits survive a logout/login cycle.
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

func isValidRedirectPath(path string) bool {
	parsedURL, err := url.Parse(path)
	if err != nil {
		return false
	}
	if parsedURL.IsAbs() || strings.Contains(path, "//") || strings.Contains(path, "..") {
		return false
	}
	return true
}
