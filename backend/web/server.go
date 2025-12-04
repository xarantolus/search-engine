package web

import (
	"bytes"
	"html/template"
	"log"
	"net/http"
	"os"
	"shared/config"
	"shared/embedding"
	"strconv"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/filesystem"
	"github.com/gofiber/fiber/v2/middleware/session"
	"github.com/gofiber/storage/sqlite3"
	"github.com/meilisearch/meilisearch-go"
	"golang.org/x/oauth2"
)

type Server struct {
	Logger *log.Logger
	Port   uint16
	Config config.Config

	Embedder *embedding.EmbedConfig

	Index meilisearch.IndexManager

	GitLab config.GitLabInfo

	// This lock is used when we do larger changes (e.g. on user signup/login),
	// as we store all user IDs in a string (lmao).
	fiberStoreLock sync.RWMutex
	FiberStore     *sqlite3.Storage

	Session     *session.Store
	OauthConfig *oauth2.Config
}

func (s *Server) Run() (err error) {
	app := fiber.New(fiber.Config{
		AppName: s.Config.AppName,
	})

	// Auth related routes
	app.Get("/login", s.LoginRoute)
	app.Get("/callback", s.LoginCallback)

	app.Use(s.LoginMiddleware)
	app.Get("/logout", s.LogoutRoute)
	app.Post("/api/v1/search", s.Search)
	app.Get("/api/v1/stats", s.IndexStats)
	app.Get("/api/v1/summary", s.DocumentStats)

	app.Get("/api/v1/docs", s.Documents)
	app.Get("/api/v1/doc/:id", s.Document)
	app.Delete("/api/v1/doc/:id", s.DeleteDocument)
	app.Get("/api/v1/text/:id", s.DocumentContent)

	app.Get("/api/v1/admin/permissions", s.AdminListPermissions)
	app.Post("/api/v1/admin/permissions", s.AdminSetPermissions)

	frontend := os.DirFS("../frontend/dist")
	const templateName = "index.html"
	t, err := template.New(templateName).ParseFS(frontend, "index.html")
	if err != nil {
		log.Fatal("Could not parse index.html from embedded filesystem")
	}
	var idx bytes.Buffer
	// execute template
	err = t.ExecuteTemplate(&idx, templateName, struct {
		AppName string
	}{
		AppName: s.Config.AppName,
	})
	if err != nil {
		log.Fatalf("Could not render index.html: %v", err)
	}
	indexFile := idx.Bytes()
	var indexHandler = func(c *fiber.Ctx) error {
		c.Set("Content-Type", "text/html")
		c.Set("Content-Length", strconv.Itoa(len(indexFile)))
		return c.Status(http.StatusOK).Send(indexFile)
	}
	app.Use(func(c *fiber.Ctx) error {
		cacheDuration := 4 * time.Hour
		c.Set("Cache-Control", "public, max-age="+strconv.Itoa(int(cacheDuration.Seconds()))+", immutable")
		c.Set("Expires", time.Now().Add(cacheDuration).Format(http.TimeFormat))
		return c.Next()
	})
	app.Get("/", indexHandler)
	app.Use("/", filesystem.New(filesystem.Config{
		Root: http.FS(frontend),
	}))
	app.Get("/*", indexHandler)

	return app.Listen(":" + strconv.FormatInt(int64(s.Port), 10))
}
