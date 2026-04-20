package web

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"shared/config"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/session"
	"github.com/gofiber/storage/sqlite3"
)

// fakeIDP is a minimal OIDC provider used to drive auth_oidc.go end-to-end
// without external services. Per-test knobs (idTokenOverride, omitGroups,
// userinfoGroups, signWithDifferentKey) are set by each table case before
// the callback request is fired.
type fakeIDP struct {
	mu sync.Mutex

	server *httptest.Server
	key    *rsa.PrivateKey
	wrong  *rsa.PrivateKey // alternative key used for "bad signature" test
	kid    string

	clientID string
	username string

	// Override knobs reset between tests.
	groups               []string
	signWithDifferentKey bool
	expDelta             time.Duration // added to time.Now() to set exp; default +5m
	overrideNonce        string        // when set, replaces the nonce we saw at /authorize
	omitGroupsFromIDTok  bool          // for userinfo-fallback testing
	userinfoGroups       []string      // groups returned by /userinfo
	overrideAud          string        // when set, replaces aud claim

	lastNonce string // captured from /authorize so /token can echo it in id_token
}

func newFakeIDP(t *testing.T, clientID, username string) *fakeIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	wrong, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa wrong key: %v", err)
	}
	idp := &fakeIDP{
		key:      key,
		wrong:    wrong,
		kid:      "test-kid",
		clientID: clientID,
		username: username,
		expDelta: 5 * time.Minute,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", idp.handleDiscovery)
	mux.HandleFunc("/jwks", idp.handleJWKS)
	mux.HandleFunc("/authorize", idp.handleAuthorize)
	mux.HandleFunc("/token", idp.handleToken)
	mux.HandleFunc("/userinfo", idp.handleUserinfo)
	idp.server = httptest.NewServer(mux)
	t.Cleanup(idp.server.Close)
	return idp
}

func (i *fakeIDP) URL() string { return i.server.URL }

func (i *fakeIDP) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	doc := map[string]any{
		"issuer":                                i.server.URL,
		"authorization_endpoint":                i.server.URL + "/authorize",
		"token_endpoint":                        i.server.URL + "/token",
		"userinfo_endpoint":                     i.server.URL + "/userinfo",
		"jwks_uri":                              i.server.URL + "/jwks",
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(doc)
}

func (i *fakeIDP) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	pub := i.key.PublicKey
	jwks := map[string]any{
		"keys": []map[string]any{{
			"kty": "RSA",
			"use": "sig",
			"alg": "RS256",
			"kid": i.kid,
			"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(jwks)
}

// handleAuthorize would normally redirect the user back to the relying
// party's redirect_uri. We never actually hit it from the test (the test
// extracts state/nonce out of the /login redirect URL directly), but it
// still records the nonce so signIDToken can echo it.
func (i *fakeIDP) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	i.mu.Lock()
	i.lastNonce = r.URL.Query().Get("nonce")
	i.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (i *fakeIDP) handleToken(w http.ResponseWriter, _ *http.Request) {
	i.mu.Lock()
	defer i.mu.Unlock()

	nonce := i.lastNonce
	if i.overrideNonce != "" {
		nonce = i.overrideNonce
	}

	now := time.Now()
	claims := map[string]any{
		"iss":                i.server.URL,
		"sub":                "user-sub",
		"aud":                i.clientID,
		"exp":                now.Add(i.expDelta).Unix(),
		"iat":                now.Unix(),
		"nbf":                now.Unix(),
		"nonce":              nonce,
		"preferred_username": i.username,
		"name":               "Test User",
		"email":              i.username + "@example.com",
	}
	if i.overrideAud != "" {
		claims["aud"] = i.overrideAud
	}
	if !i.omitGroupsFromIDTok {
		claims["groups"] = i.groups
	}

	signKey := i.key
	if i.signWithDifferentKey {
		signKey = i.wrong
	}

	idTok, err := signRS256(signKey, i.kid, claims)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	resp := map[string]any{
		"access_token": "fake-access",
		"token_type":   "Bearer",
		"id_token":     idTok,
		"expires_in":   3600,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (i *fakeIDP) handleUserinfo(w http.ResponseWriter, _ *http.Request) {
	i.mu.Lock()
	defer i.mu.Unlock()

	body := map[string]any{
		"sub":                "user-sub",
		"preferred_username": i.username,
		"name":               "Test User",
		"email":              i.username + "@example.com",
	}
	if i.userinfoGroups != nil {
		body["groups"] = i.userinfoGroups
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// signRS256 emits a compact JWS using RSASSA-PKCS1-v1_5 + SHA-256. Pulling
// in a JWT library just for the test felt heavy; this is ~10 lines.
func signRS256(key *rsa.PrivateKey, kid string, claims map[string]any) (string, error) {
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid}
	hdrJSON, _ := json.Marshal(header)
	claimsJSON, _ := json.Marshal(claims)
	signingInput := base64.RawURLEncoding.EncodeToString(hdrJSON) + "." +
		base64.RawURLEncoding.EncodeToString(claimsJSON)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// oidcTestRig wires a fakeIDP, a fresh sqlite3 store, a Fiber app with the
// oidcAuth provider mounted, and exposes a doFlow() that walks /login →
// /callback.
type oidcTestRig struct {
	t            *testing.T
	idp          *fakeIDP
	cfg          *config.Config
	app          *fiber.App
	store        *sqlite3.Storage
	provider     AuthProvider
	sessionStore *session.Store
}

func newOIDCRig(t *testing.T, allowedGroups, adminUsers []string) *oidcTestRig {
	t.Helper()

	const username = "alice"
	idp := newFakeIDP(t, "the-client", username)

	cfg := &config.Config{
		PermissionGroups: []config.PermissionGroup{
			{Name: "Default Group", Default: true, Tags: []string{"x"}},
		},
		AdminUsers: adminUsers,
		Auth: config.Auth{
			Provider: config.AuthProviderOIDC,
			OIDC: config.AuthOIDC{
				IssuerURL:     idp.URL(),
				ClientID:      "the-client",
				ClientSecret:  "secret",
				RedirectURL:   "http://localhost/callback",
				AllowedGroups: allowedGroups,
				Scopes:        []string{"openid", "profile", "email", "groups"},
			},
		},
	}

	dbFile := t.TempDir() + "/sessions.db"
	store := sqlite3.New(sqlite3.Config{
		Database: dbFile,
		Table:    "fiber_storage",
	})
	t.Cleanup(func() { _ = store.Close() })

	sessionStore := session.New(session.Config{
		Storage:        store,
		Expiration:     time.Hour,
		CookieHTTPOnly: true,
	})

	users := NewUserIndex(store)
	provider, err := NewOIDCAuth(t.Context(), cfg, sessionStore, store, users)
	if err != nil {
		t.Fatalf("NewOIDCAuth: %v", err)
	}

	app := fiber.New()
	app.Get("/login", provider.LoginRoute)
	app.Get("/callback", provider.LoginCallback)

	return &oidcTestRig{
		t:            t,
		idp:          idp,
		cfg:          cfg,
		app:          app,
		store:        store,
		provider:     provider,
		sessionStore: sessionStore,
	}
}

// doFlow runs GET /login, parses out state+nonce from the redirect URL,
// hands the nonce to the fake IdP (so it can echo it in the id_token), then
// runs GET /callback?code=...&state=... carrying the cookies set by /login.
//
// The optional tweak hook lets each test case mutate the IdP between the
// two requests (e.g. to expire the token or skip the groups claim).
func (r *oidcTestRig) doFlow(tweak func(*fakeIDP)) (*http.Response, error) {
	r.t.Helper()

	loginReq := httptest.NewRequest(http.MethodGet, "/login", nil)
	loginResp, err := r.app.Test(loginReq, -1)
	if err != nil {
		return nil, fmt.Errorf("login request: %w", err)
	}
	if loginResp.StatusCode != http.StatusFound {
		return loginResp, fmt.Errorf("login returned %d, expected 302", loginResp.StatusCode)
	}

	loc := loginResp.Header.Get("Location")
	parsed, err := url.Parse(loc)
	if err != nil {
		return nil, fmt.Errorf("parse Location: %w", err)
	}
	state := parsed.Query().Get("state")
	nonce := parsed.Query().Get("nonce")
	if state == "" || nonce == "" {
		return nil, fmt.Errorf("Location missing state/nonce: %s", loc)
	}

	r.idp.mu.Lock()
	r.idp.lastNonce = nonce
	r.idp.mu.Unlock()

	if tweak != nil {
		tweak(r.idp)
	}

	cbURL := "/callback?code=test-code&state=" + url.QueryEscape(state)
	cbReq := httptest.NewRequest(http.MethodGet, cbURL, nil)
	for _, c := range loginResp.Cookies() {
		cbReq.AddCookie(c)
	}
	cbResp, err := r.app.Test(cbReq, -1)
	if err != nil {
		return nil, fmt.Errorf("callback request: %w", err)
	}
	return cbResp, nil
}

func TestOIDCCallback(t *testing.T) {
	tests := []struct {
		name       string
		groups     []string
		tweak      func(*fakeIDP)
		wantStatus int
	}{
		{
			name:       "happy path: user in allowed group",
			groups:     []string{"search-users"},
			wantStatus: http.StatusFound,
		},
		{
			name:       "user has no matching group",
			groups:     []string{"some-other-group"},
			wantStatus: http.StatusForbidden,
		},
		{
			name:   "groups missing from id_token, userinfo provides allowed",
			groups: []string{},
			tweak: func(i *fakeIDP) {
				i.omitGroupsFromIDTok = true
				i.userinfoGroups = []string{"search-users"}
			},
			wantStatus: http.StatusFound,
		},
		{
			name:   "groups missing everywhere",
			groups: []string{},
			tweak: func(i *fakeIDP) {
				i.omitGroupsFromIDTok = true
				i.userinfoGroups = nil
			},
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rig := newOIDCRig(t, []string{"search-users"}, nil)
			rig.idp.groups = tt.groups
			resp, err := rig.doFlow(tt.tweak)
			if err != nil {
				t.Fatalf("doFlow: %v", err)
			}
			if resp.StatusCode != tt.wantStatus {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want %d (body: %s)", resp.StatusCode, tt.wantStatus, body)
			}
		})
	}
}

func TestOIDCCallback_TokenRejections(t *testing.T) {
	tests := []struct {
		name  string
		tweak func(*fakeIDP)
	}{
		{
			name: "expired id_token",
			tweak: func(i *fakeIDP) {
				i.expDelta = -time.Minute
			},
		},
		{
			name: "wrong audience",
			tweak: func(i *fakeIDP) {
				i.overrideAud = "someone-else"
			},
		},
		{
			name: "id_token signed with unknown key",
			tweak: func(i *fakeIDP) {
				i.signWithDifferentKey = true
			},
		},
		{
			name: "id_token nonce does not match session",
			tweak: func(i *fakeIDP) {
				i.overrideNonce = "wrong-nonce"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rig := newOIDCRig(t, []string{"search-users"}, nil)
			rig.idp.groups = []string{"search-users"}
			resp, err := rig.doFlow(tt.tweak)
			if err != nil {
				t.Fatalf("doFlow: %v", err)
			}
			// Verifier rejections come back as a 500 (the handler returns
			// an error to Fiber, which renders 500). Nonce mismatch is an
			// explicit 403. Either way, the redirect path *cannot* fire.
			if resp.StatusCode == http.StatusFound {
				t.Fatalf("expected rejection, got 302 redirect")
			}
		})
	}
}

func TestOIDCCallback_StateMismatch(t *testing.T) {
	rig := newOIDCRig(t, []string{"search-users"}, nil)
	rig.idp.groups = []string{"search-users"}

	loginReq := httptest.NewRequest(http.MethodGet, "/login", nil)
	loginResp, err := rig.app.Test(loginReq, -1)
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	cbReq := httptest.NewRequest(http.MethodGet, "/callback?code=x&state=tampered", nil)
	for _, c := range loginResp.Cookies() {
		cbReq.AddCookie(c)
	}
	resp, err := rig.app.Test(cbReq, -1)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestOIDCCallback_AdminFlag(t *testing.T) {
	rig := newOIDCRig(t, []string{"search-users"}, []string{"alice"})
	rig.idp.groups = []string{"search-users"}

	resp, err := rig.doFlow(nil)
	if err != nil {
		t.Fatalf("doFlow: %v", err)
	}
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}

	keys, err := rig.provider.ListUserKeys()
	if err != nil {
		t.Fatalf("ListUserKeys: %v", err)
	}
	wantKey := "oidc:alice"
	if len(keys) != 1 || keys[0] != wantKey {
		t.Fatalf("ListUserKeys = %v, want [%q]", keys, wantKey)
	}

	cookieReq := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range resp.Cookies() {
		cookieReq.AddCookie(c)
	}

	var captured UserInfo
	probe := fiber.New()
	probe.Use(func(c *fiber.Ctx) error {
		info, err := rig.provider.UserInfo(c)
		if err != nil {
			return err
		}
		captured = info
		return c.SendStatus(fiber.StatusOK)
	})
	probeReq := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range resp.Cookies() {
		probeReq.AddCookie(c)
	}
	if _, err := probe.Test(probeReq, -1); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !captured.IsAdmin {
		t.Fatalf("expected IsAdmin=true for user in admin_users, got %+v", captured)
	}
	if !strings.HasPrefix(captured.UserKey, "oidc:") {
		t.Fatalf("UserKey = %q, want oidc:* prefix", captured.UserKey)
	}
}
