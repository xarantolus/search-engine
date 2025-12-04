package scrapers

import (
	"bytes"
	"context"
	"fmt"
	"indexer/scrapers/extractors"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"shared/config"
	"shared/doc"
	"shared/embedding"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/dustin/go-humanize"
	"github.com/geziyor/geziyor"
	"github.com/geziyor/geziyor/cache/diskcache"
	"github.com/geziyor/geziyor/client"
	"github.com/google/go-tika/tika"
	"github.com/meilisearch/meilisearch-go"
	"golang.org/x/net/publicsuffix"
)

type ScrapeWebsiteJob struct {
	Name      string
	Website   config.Website
	parsedURL *url.URL

	Index      meilisearch.IndexManager
	TikaClient *tika.Client

	Embedder *embedding.EmbedConfig
	Config   *config.Config

	IntervalHelper
}

func (s *ScrapeWebsiteJob) DisplayName() string {
	return fmt.Sprintf("Website:%s (%s)", s.Name, s.Website.URL)
}

func (s *ScrapeWebsiteJob) Setup() (err error) {
	s.parsedURL, err = url.Parse(s.Website.URL)
	if err != nil {
		return
	}

	return
}

func (s *ScrapeWebsiteJob) Run() (err error) {
	logger := log.New(os.Stdout, fmt.Sprintf("[ScrapeWebsite:%s] ", s.Name), log.LstdFlags)
	logger.Printf("Scraping website %s", s.Website.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var expirationChecker = NewExpirationChecker(ctx, s.Embedder, logger, s.Index, s.Config.DeleteAfterTime, false)

	// Create document channel and batcher
	documentChannel := make(chan doc.Document, 100)
	var documentBatcher = doc.NewDocumentBatcher(s.Index, s.Embedder, s.Config.MaxDocumentSize, doc.MEILISEARCH_SAFE_MAX_COMMIT_SIZE, s.Config.MinCommit, logger)

	// Start document processing goroutine
	done := make(chan struct{})
	go func() {
		defer close(done)
		for pageDoc := range documentChannel {
			numCommitted, err := documentBatcher.AddDocument(pageDoc)
			if err == nil {
				logger.Printf("Staged page %s (%s)", pageDoc.Slug, humanize.Bytes(uint64(len(pageDoc.Content))))
			} else {
				logger.Printf("Failed to add page %s: %v", pageDoc.ID, err)
			}
			if numCommitted > 0 {
				logger.Printf("Committed %d pages", numCommitted)
			}
		}
	}()

	// for e.g. www.example.com, blog.example.com etc. get example.com as the top-level host
	// with regard to the public suffix, so that we can scrape all subdomains
	etldPlusOne, err := publicsuffix.EffectiveTLDPlusOne(s.parsedURL.Hostname())
	if err != nil {
		etldPlusOne = s.parsedURL.Hostname()
	}
	var extractor = extractors.ForStream(s.TikaClient)

	var parseFunc = func(g *geziyor.Geziyor, r *client.Response) {
		// First of all, try to extract content via Tika
		if r.StatusCode >= 200 && r.StatusCode < 300 {
			r.Request.URL.Fragment = "" // Remove fragment to avoid duplicate URLs
			var docID = hashURL(r.Request.URL.String())

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			textContent, err := extractor.ExtractTextStream(ctx, docID, bytes.NewReader(r.Body), r.Header.Get("Content-Type"))
			cancel()
			if err != nil {
				logger.Printf("Failed to extract text from %s: %v\n", r.Request.URL.String(), err)
				textContent = ""
			}
			var title string
			if r.HTMLDoc != nil {
				title = r.HTMLDoc.Find("title").Text()
			} else {
				// First line of the content as title if HTMLDoc is not available
				lines := strings.SplitN(textContent, "\n", 2)
				if len(lines) > 0 {
					title = strings.TrimSpace(lines[0])
				}
			}

			// in seconds
			var modificationUnix int64
			if date, ok := r.Response.Header["Last-Modified"]; ok && len(date) > 0 {
				modificationTime, err := time.Parse(time.RFC1123, date[0])
				if err == nil {
					modificationUnix = modificationTime.Unix()
				}
			}
			// Try date header
			if modificationUnix == 0 {
				if date, ok := r.Response.Header["Date"]; ok && len(date) > 0 {
					modificationTime, err := time.Parse(time.RFC1123, date[0])
					if err == nil {
						modificationUnix = modificationTime.Unix()
					}
				}
			}
			// If still no modification time, use current time
			if modificationUnix == 0 {
				modificationUnix = time.Now().Unix()
			}

			exist, _, err := expirationChecker.IsDocUpToDate(docID, modificationUnix, "website", s.Website.PermissionTag)
			if err != nil || !exist {
				// Build slug with query parameters only if they exist
				var slug = path.Join(s.Name, r.Response.Request.URL.Path)
				if r.Response.Request.URL.RawQuery != "" {
					slug += "?" + r.Response.Request.URL.RawQuery
				}

				var pageDoc = doc.Document{
					URL:           r.Response.Request.URL.String(),
					ID:            docID,
					Content:       textContent,
					Slug:          slug,
					Title:         title,
					LastModified:  doc.FloatyNumber(modificationUnix),
					Version:       doc.DocumentVersion,
					ReIndex:       false,
					IsCode:        false,
					Extractor:     "website",
					PermissionTag: s.Website.PermissionTag,
				}

				documentChannel <- pageDoc
			}
		}

		// Extract links and visit them
		if r.HTMLDoc != nil {
			r.HTMLDoc.Find("a[href]").Each(func(i int, selection *goquery.Selection) {
				href, exists := selection.Attr("href")
				if !exists || href == "" {
					return
				}

				absoluteURL, err := r.Request.URL.Parse(href)
				if err != nil {
					logger.Printf("Failed to parse URL %s: %v\n", href, err)
					return
				}

				absoluteURL.Fragment = "" // Remove fragment to avoid duplicate URLs

				targetDomain, err := publicsuffix.EffectiveTLDPlusOne(absoluteURL.Host)
				if err != nil || targetDomain != etldPlusOne {
					return
				}

				g.Get(absoluteURL.String(), g.Opt.ParseFunc)
			})

			// Extract assets (images, embeds, iframes)
			assetSelectors := []string{"img[src]", "embed[src]", "iframe[src]", "object[data]"}

			for _, selector := range assetSelectors {
				r.HTMLDoc.Find(selector).Each(func(i int, selection *goquery.Selection) {
					var attrName string
					switch {
					case strings.Contains(selector, "[src]"):
						attrName = "src"
					case strings.Contains(selector, "[data]"):
						attrName = "data"
					default:
						return
					}

					href, exists := selection.Attr(attrName)
					if !exists || href == "" {
						return
					}

					absoluteURL, err := r.Request.URL.Parse(href)
					if err != nil {
						logger.Printf("Failed to parse asset URL %s: %v\n", href, err)
						return
					}

					absoluteURL.Fragment = "" // Remove fragment to avoid duplicate URLs
					g.Get(absoluteURL.String(), g.Opt.ParseFunc)
				})
			}
		}
	}

	// Create Geziyor instance
	geziyorOptions := &geziyor.Options{
		StartRequestsFunc: func(g *geziyor.Geziyor) {
			g.Get(s.parsedURL.String(), parseFunc)
		},
		ParseFunc:                   parseFunc,
		ConcurrentRequests:          4,
		ConcurrentRequestsPerDomain: 2,
		RequestDelay:                1 * time.Second,
		RequestDelayRandomize:       true,
		Timeout:                     30 * time.Second,
		MaxBodySize:                 100 * 1024 * 1024, // 100 MB
		LogDisabled:                 true,
		Cache:                       diskcache.New(filepath.Join(s.Config.Cache.Dir, "websites", s.Name, s.parsedURL.Hostname())),
		RequestsPerSecond:           2,
		RobotsTxtDisabled:           false,
		UserAgent:                   "Mozilla/5.0 (compatible; SearchEngine/1.0)",
		URLRevisitEnabled:           false,
		// This ensures that the scraper does not corrupt e.g. PDF files
		CharsetDetectDisabled: true,
	}

	// Create and start Geziyor
	g := geziyor.NewGeziyor(geziyorOptions)
	g.Client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}

		topLevel, err := publicsuffix.EffectiveTLDPlusOne(req.URL.Hostname())
		if err != nil {
			topLevel = req.URL.Hostname()
		}
		if topLevel != etldPlusOne {
			return fmt.Errorf("redirected to a different domain: %s", req.URL.Hostname())
		}

		return nil
	}
	g.Start()

	// Close document channel and wait for processing to complete
	close(documentChannel)
	<-done

	// Final commit of any remaining documents
	numCommitted, err := documentBatcher.Commit()
	if err != nil {
		return fmt.Errorf("%s: failed to commit documents: %w", s.Name, err)
	}
	if numCommitted > 0 {
		logger.Printf("Final commit: %d documents", numCommitted)
	}

	return nil
}
