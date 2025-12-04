package main

import (
	"context"
	"flag"
	"indexer/scrapers"
	"log"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"shared/config"
	"shared/embedding"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/go-tika/tika"
	"github.com/meilisearch/meilisearch-go"
	"github.com/mitchellh/copystructure"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

func main() {
	var (
		flagConfigPath   = flag.String("c", "config.yml", "Path to the configuration file")
		flagForceReindex = flag.Bool("index-update", false, "Force Meilisearch to fix entries sorted wrong")
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

	tikaHost := os.Getenv("TIKA_HOST")
	if tikaHost == "" {
		log.Fatal("TIKA_HOST environment variable must be set")
	}

	tikaClient := tika.NewClient(httpClient, tikaHost)

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
		if maxLenStr == "" {
			maxLen = int64(cfg.MaxDocumentSize)
		} else {
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

	log.Printf("Checking index %q...", cfg.IndexName)

	_, err = client.GetIndex(cfg.IndexName)
	if err != nil {
		if !strings.Contains(err.Error(), "index_not_found") {
			log.Fatalf("Failed to get index: %v", err)
		}

		log.Printf("Index %q not found, creating...", cfg.IndexName)

		createTask, err := client.CreateIndex(&meilisearch.IndexConfig{
			PrimaryKey: "id",
			Uid:        cfg.IndexName,
		})
		if err != nil && !strings.Contains(err.Error(), "already exists") {
			log.Fatalf("Failed to create index: %v", err)
		}

		_, err = client.WaitForTask(createTask.TaskUID, 0)
		if err != nil {
			log.Fatalf("Failed to wait for index creation: %v", err)
		}
	}

	index := client.Index(cfg.IndexName)

	currentSettings, err := index.GetSettings()
	if err != nil {
		log.Fatalf("Failed to get index settings: %v", err)
	}

	originalSettingsInterface, err := copystructure.Copy(currentSettings)
	if err != nil {
		log.Fatalf("Failed to copy current settings: %v", err)
	}
	originalSettings := originalSettingsInterface.(*meilisearch.Settings)

	var settingsChanged bool = *flagForceReindex
	// Note: order is important for searchable attributes, as it defines ranking importance
	currentSettings.SearchableAttributes = []string{"title", "content", "slug", "url"}
	settingsChanged = settingsChanged || !reflect.DeepEqual(originalSettings.SearchableAttributes, currentSettings.SearchableAttributes)
	currentSettings.SortableAttributes = []string{"lastModified"}
	settingsChanged = settingsChanged || !reflect.DeepEqual(originalSettings.SortableAttributes, currentSettings.SortableAttributes)
	// Note: this must be sorted alphabetically, as meili sorts them on its end and then the comparison gets the wrong result later
	currentSettings.FilterableAttributes = []string{"contentSize", "indexTime", "isCode", "lastModified", "permissionTag"}
	settingsChanged = settingsChanged || !reflect.DeepEqual(originalSettings.FilterableAttributes, currentSettings.FilterableAttributes)
	currentSettings.Synonyms = cfg.Synonyms.Merged
	settingsChanged = settingsChanged || !reflect.DeepEqual(originalSettings.Synonyms, currentSettings.Synonyms)
	currentSettings.Dictionary = cfg.Dictionary
	settingsChanged = settingsChanged || !reflect.DeepEqual(originalSettings.Dictionary, currentSettings.Dictionary)
	currentSettings.RankingRules = []string{
		"sort",
		"words",
		"typo",
		"proximity",
		"attribute",
		"exactness",
		// Prefer newer documents
		"lastModified:desc",
	}
	settingsChanged = settingsChanged || !reflect.DeepEqual(originalSettings.RankingRules, currentSettings.RankingRules)

	if settingsChanged {
		log.Printf("Updating index settings...")
		settingsTask, err := index.UpdateSettings(currentSettings)
		if err != nil {
			log.Fatalf("Failed to update index settings: %v", err)
		}

		_, err = client.WaitForTask(settingsTask.TaskUID, 0)
		if err != nil {
			log.Fatalf("Failed to wait for index settings to apply: %v", err)
		}
	}

	if useAISearch {
		log.Printf("AI search is enabled - configuring embedder...")
		// Create or update our custom embedder model
		embedders, err := index.GetEmbedders()
		if err != nil {
			log.Fatalf("Failed to get embedders: %v", err)
		}

		testVec, err := embedConfig.EmbedText("test", false)
		if err != nil {
			log.Fatalf("Failed to test embedder: %v", err)
		}
		embedConfig.Dimensions = len(testVec)

		if _, exists := embedders[embedConfig.EmbedModel]; !exists || len(embedders) != 1 {
			embedders = map[string]meilisearch.Embedder{
				embedConfig.EmbedModel: {
					Source:     "userProvided",
					Dimensions: embedConfig.Dimensions,
				},
			}

			log.Printf("Updating embedder %q with dimensions %d", embedConfig.EmbedModel, embedConfig.Dimensions)
			task, err := index.UpdateEmbedders(embedders)
			if err != nil {
				log.Fatalf("Failed to update embedders: %v", err)
			}

			_, err = client.WaitForTask(task.TaskUID, 0)
			if err != nil {
				log.Fatalf("Failed to wait for embedder update: %v", err)
			}
		}
	} else {
		// If AI search is disabled, delete all embedders
		embedders, err := index.GetEmbedders()
		if err != nil {
			log.Fatalf("Failed to get embedders: %v", err)
		}

		if len(embedders) > 0 {
			log.Printf("AI search is disabled - resetting embedders")
			task, err := index.ResetEmbedders()
			if err != nil {
				log.Fatalf("Failed to delete embedders: %v", err)
			}

			_, err = client.WaitForTask(task.TaskUID, 0)
			if err != nil {
				log.Fatalf("Failed to wait for embedder deletion: %v", err)
			}
		}
	}

	log.Printf("Creating background tasks...")

	var scraperJobs []scrapers.Job

	for name, group := range cfg.GitlabGroups {
		// Some group definitions are only for repo indexing
		if !(group.ScrapeIssues || group.ScrapeMergeRequests) {
			continue
		}

		scraperJobs = append(scraperJobs, &scrapers.ScrapeGitLabGroupJob{
			Name:   name,
			Group:  group,
			Index:  client.Index(cfg.IndexName),
			Config: &cfg,
			IntervalHelper: scrapers.IntervalHelper{
				Interval: group.Interval,
			},
			Embedder: embedConfig,
		})
	}

	for name, repo := range cfg.Repositories {
		scraperJobs = append(scraperJobs, &scrapers.ScrapeGitJob{
			Name:       name,
			Repo:       repo,
			Index:      client.Index(cfg.IndexName),
			Config:     &cfg,
			TikaClient: tikaClient,
			IntervalHelper: scrapers.IntervalHelper{
				Interval: repo.Interval,
			},
			Embedder: embedConfig,
		})

		if repo.IgnoreIssuesPRs {
			continue
		}

		scraperJobs = append(scraperJobs, &scrapers.ScrapeGitLabMetaJob{
			Name:   name,
			Repo:   repo,
			Index:  client.Index(cfg.IndexName),
			Config: &cfg,
			IntervalHelper: scrapers.IntervalHelper{
				Interval: repo.Interval,
			},
			Embedder: embedConfig,
		})
	}

	for name, mount := range cfg.Mounts {
		scraperJobs = append(scraperJobs, &scrapers.MountPointJob{
			Name:       scrapers.Cascade(mount.NameOverride, name),
			Index:      client.Index(cfg.IndexName),
			MountPoint: mount,
			Config:     &cfg,
			TikaClient: tikaClient, IntervalHelper: scrapers.IntervalHelper{
				Interval: mount.Interval,
			},
			Embedder: embedConfig,
		})
	}

	for name, confluence := range cfg.Conflucence {
		scraperJobs = append(scraperJobs, &scrapers.ScrapeConfluenceJob{
			Name:       name,
			Confluence: &confluence,
			Config:     &cfg,
			TikaClient: tikaClient,
			Index:      client.Index(cfg.IndexName),
			IntervalHelper: scrapers.IntervalHelper{
				Interval: confluence.Interval,
			},
			Embedder: embedConfig,
		})
	}

	for name, website := range cfg.Websites {
		scraperJobs = append(scraperJobs, &scrapers.ScrapeWebsiteJob{
			Name:       name,
			Website:    website,
			Index:      client.Index(cfg.IndexName),
			Config:     &cfg,
			Embedder:   embedConfig,
			TikaClient: tikaClient,
			IntervalHelper: scrapers.IntervalHelper{
				Interval: website.Interval,
			},
		})
	}

	// This job deletes elements from the search index if they have not been updated in a while,
	// getting rid of docs that have been deleted in the source.
	scraperJobs = append(scraperJobs, &scrapers.ExpirationJob{
		Index:     client.Index(cfg.IndexName),
		OlderThan: cfg.DeleteAfterTime,
	})

	// We also want to clean up our cache directory. If a file doesn't change, only its text cache entry will be read,
	// and the source file can be deleted. The text cache is also cleaned from documents that are no longer in the index.
	scraperJobs = append(scraperJobs, &scrapers.CacheCleanJob{
		FileCacheOlderThan: scrapers.ExpirationMargin(cfg.DeleteAfterTime, 2),
		TextCacheOlderThan: scrapers.ExpirationMargin(cfg.DeleteAfterTime, 2),
		CacheConfig:        cfg.Cache,
	})

	log.Printf("Setting up %d scraper jobs...", len(scraperJobs))
	var setupJobs = func(jbs []scrapers.Job) {
		for i, job := range jbs {
			if setupJob, ok := job.(scrapers.SetupJob); ok {
				jbs[i] = scrapers.MaybeWrapSetupJob(setupJob)
				if err := setupJob.Setup(); err != nil {
					log.Printf("Failed to setup job %T: %v", job, err)
				}
			}
		}
	}
	setupJobs(scraperJobs)

	// Find all gitlab repo jobs and get their gitlab ID
	var indexedRepoIDs = map[int64]struct{}{}
	for _, job := range scraperJobs {
		j := job
		if unwrapped, ok := job.(*scrapers.SetupJobWrapper); ok {
			j = unwrapped.SetupJob
		}

		if gitlabJob, ok := j.(*scrapers.ScrapeGitJob); ok {
			if gitlabJob.Repo.IsWiki {
				continue
			}

			if gitlabJob.GitLabID == 0 {
				panic("GitLabID is 0 - should have been set by setup")
			}

			indexedRepoIDs[gitlabJob.GitLabID] = struct{}{}
		}
	}

	var initializedScraperJobs = len(scraperJobs)

	// List all repos in group + all subgroups of our group
	var o sync.Once
	for name, group := range cfg.GitlabGroups {
		if !group.IndexRepos {
			continue
		}
		o.Do(func() {
			log.Printf("Creating GitLab-Specific jobs...")
		})

		glc, err := scrapers.GitLabClientFromURL(&cfg, group.Host)
		if err != nil {
			log.Fatalf("Failed to create GitLab client: %v", err)
		}

		projects, err := scrapers.ListGitLabGroupProjects(glc, group.ID)
		if err != nil {
			log.Fatalf("Failed to list GitLab group projects: %v", err)
		}

		log.Printf("Checking jobs for %d projects found in %q", len(projects), name)

		for _, project := range projects {
			if _, ok := indexedRepoIDs[int64(project.ID)]; ok {
				continue
			}

			if len(group.IndexReposExcludeGlob) > 0 && scrapers.AnyGlobMatches(group.IndexReposExcludeGlob, project.PathWithNamespace) {
				log.Printf("Skipping %q due to index_repos_exclude match", project.PathWithNamespace)
				continue
			}

			if len(group.IndexReposIncludeGlob) > 0 && !scrapers.AnyGlobMatches(group.IndexReposIncludeGlob, project.PathWithNamespace) {
				log.Printf("Skipping %q as it is not included in index_repos_include match", project.PathWithNamespace)
				continue
			}

			scraperJobs = append(scraperJobs, &scrapers.ScrapeGitJob{
				Name: project.PathWithNamespace,
				Repo: config.Repository{
					URL:             project.HTTPURLToRepo,
					IgnoreIssuesPRs: true,
					IsWiki:          false,
					Interval:        group.IndexReposInterval,
					PermissionTag:   scrapers.Cascade(group.IndexReposPermissionTagOverride, group.PermissionTag),
				},
				Index:      client.Index(cfg.IndexName),
				Config:     &cfg,
				TikaClient: tikaClient,
				IntervalHelper: scrapers.IntervalHelper{
					Interval: group.IndexReposInterval,
				},
				GitLabID: int64(project.ID),
				Embedder: embedConfig,
			})

			if group.IndexReposIgnoreIssuesPRs {
				continue
			}

			scraperJobs = append(scraperJobs, &scrapers.ScrapeGitLabMetaJob{
				Name: project.PathWithNamespace,
				Repo: config.Repository{
					URL:             project.HTTPURLToRepo,
					IgnoreIssuesPRs: false,
					IsWiki:          false,
					Interval:        group.IndexReposInterval,
					PermissionTag:   scrapers.Cascade(group.IndexReposPermissionTagOverride, group.PermissionTag),
				},
				Index:  client.Index(cfg.IndexName),
				Config: &cfg,
				IntervalHelper: scrapers.IntervalHelper{
					Interval: group.Interval,
				},
				Embedder: embedConfig,
			})
		}
	}

	if len(scraperJobs) != initializedScraperJobs {
		log.Printf("Setting up %d new GitLab-specific jobs...", len(scraperJobs)-initializedScraperJobs)
	}

	setupJobs(scraperJobs[initializedScraperJobs:])

	log.Printf("Finished setup, starting indexer!")
	scrapers.Run(&cfg, scraperJobs)
}
