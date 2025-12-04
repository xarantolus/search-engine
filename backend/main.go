package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"net/url"
	"os"
	"searccher/web"
	"shared/config"
	"shared/embedding"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2/middleware/session"
	"github.com/gofiber/storage/sqlite3"
	"github.com/meilisearch/meilisearch-go"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"golang.org/x/oauth2"
)

func main() {
	var (
		flagConfigPath = flag.String("c", "config.yml", "Path to the configuration file")
	)
	flag.Parse()

	cfg, err := config.Parse(*flagConfigPath)
	if err != nil {
		log.Fatalf("Failed to parse configuration: %v", err)
	}

	meiliHost, meiliAPIKey := os.Getenv("MEILI_HOST"), os.Getenv("MEILI_MASTER_KEY")
	if meiliHost == "" || meiliAPIKey == "" {
		log.Fatal("Both MEILI_HOST and MEILI_MASTER_KEY environment variables must be set")
	}

	var httpClient = &http.Client{
		Timeout: 5 * time.Minute,
	}

	client := meilisearch.New(meiliHost, meilisearch.WithAPIKey(meiliAPIKey), meilisearch.WithCustomClient(httpClient))

	gitlabInfo, err := config.GitLabFromEnv()
	if err != nil {
		log.Fatalf("Failed to get GitLab info from environment: %v", err)
	}

	log.Printf("Checking index %q...", cfg.IndexName)

	_, err = client.GetIndex(cfg.IndexName)
	if err != nil {
		log.Fatalf("Failed to get index: %v", err)
	}

	index := client.Index(cfg.IndexName)

	sps := os.Getenv("PORT")
	if sps == "" {
		sps = "8080"
	}

	serverPort, err := strconv.ParseUint(sps, 10, 16)
	if err != nil {
		log.Fatalf("Failed to parse PORT environment variable: %v", err)
	}

	sessions_db := os.Getenv("SESSIONS_DB")
	if sessions_db == "" {
		sessions_db = "./sessions.db"
	}

	fiberStore := sqlite3.New(sqlite3.Config{
		Database:        sessions_db,
		Table:           "fiber_storage",
		Reset:           false,
		GCInterval:      10 * time.Second,
		MaxOpenConns:    100,
		MaxIdleConns:    100,
		ConnMaxLifetime: 30 * time.Second,
	})

	sessionStore := session.New(session.Config{
		Storage:    fiberStore,
		Expiration: 28 * 24 * time.Hour,
		// Ensure JS cannot read out cookie, but it is still sent with requests
		CookieHTTPOnly: true,
	})

	var resolve = func(base *url.URL, path string) string {
		return base.ResolveReference(&url.URL{Path: path}).String()
	}

	var oauthConfig = &oauth2.Config{
		ClientID:     gitlabInfo.GitLabApplicationID,
		ClientSecret: gitlabInfo.GitLabApplicationSecret,
		RedirectURL:  resolve(gitlabInfo.HostExternalURL, "/callback"),
		Scopes:       []string{"read_user", "openid", "read_api"},
		Endpoint: oauth2.Endpoint{
			AuthURL:       resolve(gitlabInfo.GitLabBaseURL, "/oauth/authorize"),
			TokenURL:      resolve(gitlabInfo.GitLabBaseURL, "/oauth/token"),
			DeviceAuthURL: resolve(gitlabInfo.GitLabBaseURL, "/oauth/device/code"),
		},
	}

	useAISearch := os.Getenv("AI_SEARCH_DISABLE") != "true"
	var embedConfig *embedding.EmbedConfig
	if useAISearch {
		openAIURL, err := url.ParseRequestURI(os.Getenv("OPENAI_URL_PREFIX"))
		if err != nil {
			log.Fatalf("OPENAI_URL_PREFIX environment variable must be a valid URL: %v", err)
		}

		openAIClient := openai.NewClient(
			option.WithBaseURL(openAIURL.String()),
		)

		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		models, err := openAIClient.Models.List(ctx)
		if err != nil {
			log.Fatalf("Failed to list models: %v", err)
		}

		if len(models.Data) != 1 {
			log.Fatalf("Expected exactly one model, got %d", len(models.Data))
		}

		var maxLen int64

		// Models like rwkv have infinite length
		maxLenStr := models.Data[0].JSON.ExtraFields["max_model_len"].Raw()
		if maxLenStr != "" {
			maxLen, err = strconv.ParseInt(maxLenStr, 10, 64)
			if err != nil {
				log.Fatalf("Failed to parse max_model_len: %v", err)
			}
		}

		embedConfig = &embedding.EmbedConfig{
			EmbedModel: models.Data[0].ID,
			MaxTokens:  int(maxLen),
			Embedder:   &openAIClient,
		}
	}

	var serverLogger = log.New(os.Stdout, "", log.LstdFlags)
	serverLogger.SetPrefix("[Server] ")

	server := &web.Server{
		Logger:      serverLogger,
		Port:        uint16(serverPort),
		Config:      cfg,
		Index:       index,
		GitLab:      gitlabInfo,
		Session:     sessionStore,
		OauthConfig: oauthConfig,
		FiberStore:  fiberStore,
		Embedder:    embedConfig,
	}

	if err := server.Run(); err != nil {
		log.Fatalf("Failed to run web server: %v", err)
	}
}
