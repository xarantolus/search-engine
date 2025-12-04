package scrapers

import (
	"context"
	"fmt"
	"indexer/scrapers/extractors"
	"log"
	"os"
	"regexp"
	"shared/config"
	"shared/doc"
	"shared/embedding"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/essentialkaos/go-confluence/v6"
	"github.com/google/go-tika/tika"
	"github.com/meilisearch/meilisearch-go"
	"golang.org/x/net/html"
)

type ScrapeConfluenceJob struct {
	Name string

	Confluence *config.Confluence

	TikaClient *tika.Client
	Index      meilisearch.IndexManager

	Embedder *embedding.EmbedConfig
	logger   *log.Logger

	api    *confluence.API
	Config *config.Config
	IntervalHelper
}

func (s *ScrapeConfluenceJob) DisplayName() string {
	return fmt.Sprintf("Confluence:%s (%s)", s.Name, s.Confluence.PageBaseURL)
}

func (s *ScrapeConfluenceJob) Setup() (err error) {
	s.logger = log.New(os.Stdout, fmt.Sprintf("[Confluence:%s] ", s.Name), log.LstdFlags)

	api, err := confluence.NewAPI(s.Confluence.APIURL, confluence.AuthToken{
		Token: s.Confluence.Token,
	})
	// The default client has a limit of 3 seconds, and confluence is *slow* for fetching pages
	api.Client.ReadTimeout = 1 * time.Minute
	api.Client.WriteTimeout = 1 * time.Minute
	if err != nil {
		return fmt.Errorf("failed to create confluence API: %w", err)
	}

	s.api = api
	_, err = s.api.GetCurrentUser(confluence.ExpandParameters{})

	return
}

func (s *ScrapeConfluenceJob) Run() (err error) {
	var documentBatcher = doc.NewDocumentBatcher(s.Index, s.Embedder, s.Config.MaxDocumentSize, doc.MEILISEARCH_SAFE_MAX_COMMIT_SIZE, s.Config.MinCommit, s.logger)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var expirationChecker = NewExpirationChecker(ctx, s.Embedder, s.logger, s.Index, s.Config.DeleteAfterTime, false)

	fetchErr := s.fetchAllPages(documentBatcher, expirationChecker)

	// Some pages may have been staged but not committed
	numCommitted, err := documentBatcher.Commit()
	if err != nil {
		return fmt.Errorf("failed to commit pages: %w", err)
	}
	if numCommitted > 0 {
		s.logger.Printf("Committed %d pages", numCommitted)
	}

	if fetchErr != nil {
		return fmt.Errorf("failed to fetch confluence pages: %w", fetchErr)
	}
	return
}

// Confluence seems to be *very* slow
const maxConfluencePagesLimit = 10

func (s *ScrapeConfluenceJob) fetchAllPages(documentBatcher *doc.DocumentBatcher, expiration *ExpirationChecker) (err error) {
	var start = 0
	var extractor = extractors.ForStream(s.TikaClient)

	for {
		childPages, err := s.api.GetContent(confluence.ContentParameters{
			SpaceKey: s.Confluence.SpaceKey,
			Start:    start,
			Limit:    maxConfluencePagesLimit,
			Expand:   []string{"body.view", "children.page", "version", "ancestors"},
		})
		if err != nil || childPages.Results == nil {
			return err
		}

		for _, p := range childPages.Results {
			// For this page, create a document
			var pageURL = fmt.Sprintf("%s%s", s.Confluence.PageBaseURL, p.Links.WebUI)

			var lastMod = p.Version.When.Unix()

			var docID = hashURL(pageURL)
			exist, reason, err := expiration.IsDocUpToDate(docID, lastMod, "confluencev6", s.Confluence.PermissionTag)
			if err != nil || !exist {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				textContent, err := extractor.ExtractTextStream(ctx, docID, strings.NewReader(p.Body.View.Value))
				cancel()
				if err != nil {
					s.logger.Printf("Stripping HTML, as extraction failed: %v", err)
					textContent, err = stripHTMLTags(p.Body.View.Value)
					if err != nil {
						s.logger.Printf("Failed to strip HTML tags: %v", err)
					}
				}

				// Strip content again: sometimes we still have some HTML tags left in from tika
				tc2, err := stripHTMLTags(textContent)
				if err == nil {
					textContent = tc2
				}

				if err == nil {
					var pageDoc = doc.Document{
						URL:           pageURL,
						ID:            docID,
						Content:       textContent,
						Slug:          buildSlug(s.Name, p.Ancestors, p.Title),
						Title:         p.Title,
						LastModified:  doc.FloatyNumber(lastMod),
						Version:       doc.DocumentVersion,
						ReIndex:       false,
						IsCode:        false,
						Extractor:     "confluencev6",
						PermissionTag: s.Confluence.PermissionTag,
					}

					numCommitted, err := documentBatcher.AddDocument(pageDoc)
					if err == nil {
						s.logger.Printf("Staged page %s (%s, %s)", pageDoc.Slug, humanize.Bytes(uint64(len(pageDoc.Content))), reason)
					} else {
						s.logger.Printf("Failed to add page %s (%s): %v", docID, reason, err)
					}
					if numCommitted > 0 {
						s.logger.Printf("Committed %d pages", numCommitted)
					}
				}
			}

			// Recursively fetch child pages of our child pages
			err = s.fetchChildPages(documentBatcher, expiration, extractor, p.ID, s.Name+"/"+p.Title)
			if err != nil {
				return err
			}
		}

		if childPages.Size < maxConfluencePagesLimit {
			break
		}
		start += childPages.Limit
	}

	return nil
}

func (s *ScrapeConfluenceJob) fetchChildPages(documentBatcher *doc.DocumentBatcher, expiration *ExpirationChecker, extractor *extractors.AnyDocumentExtractor, parentPageID string, pagePathPrefix string) (err error) {
	var start = 0

	for {
		childPages, err := s.api.GetContentChildren(parentPageID, confluence.ChildrenParameters{
			Expand: []string{"body.view", "children.page", "version", "ancestors"},
			Start:  start,
			Limit:  maxConfluencePagesLimit,
		})
		if err != nil || childPages.Pages == nil {
			return err
		}

		for _, p := range childPages.Pages.Results {
			var (
				pageURL = fmt.Sprintf("%s%s", s.Confluence.PageBaseURL, strings.TrimPrefix(p.Links.WebUI, "/display"))
				docID   = hashURL(pageURL)
			)

			modDate := p.Version.When.Unix()

			exist, reason, err := expiration.IsDocUpToDate(docID, modDate, "confluencev6", s.Confluence.PermissionTag)
			if err != nil || !exist {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				textContent, err := extractor.ExtractTextStream(ctx, docID, strings.NewReader(p.Body.View.Value))
				cancel()
				if err != nil {
					s.logger.Printf("Stripping HTML, as extraction failed: %v", err)
					textContent, err = stripHTMLTags(p.Body.View.Value)
					if err != nil {
						s.logger.Printf("Failed to strip HTML tags: %v", err)
					}
				}

				// Strip content again: sometimes we still have some HTML tags left in from tika
				tc2, err := stripHTMLTags(textContent)
				if err == nil {
					textContent = tc2
				}

				if err == nil {
					var pageDoc = doc.Document{
						URL:           pageURL,
						ID:            docID,
						Content:       textContent,
						Slug:          buildSlug(s.Name, p.Ancestors, p.Title),
						Title:         p.Title,
						LastModified:  doc.FloatyNumber(modDate),
						Version:       doc.DocumentVersion,
						ReIndex:       false,
						IsCode:        false,
						Extractor:     "confluencev6",
						PermissionTag: s.Confluence.PermissionTag,
					}

					numCommitted, err := documentBatcher.AddDocument(pageDoc)
					if err == nil {
						s.logger.Printf("Staged page %s (%s, %s)", pageDoc.Slug, humanize.Bytes(uint64(len(pageDoc.Content))), reason)
					} else {
						s.logger.Printf("Failed to add page %s (%s): %v", docID, reason, err)
					}
					if numCommitted > 0 {
						s.logger.Printf("Committed %d pages", numCommitted)
					}
				}

			}

			err = s.fetchChildPages(documentBatcher, expiration, extractor, p.ID, pagePathPrefix+"/"+p.Title)
			if err != nil {
				return err
			}
		}

		if childPages.Pages.Size < maxConfluencePagesLimit {
			break
		}

		start += childPages.Pages.Limit
	}

	return nil
}

func stripHTMLTags(htmlContent string) (text string, err error) {
	doc, err := html.Parse(strings.NewReader(htmlContent))
	if err != nil {
		return "", fmt.Errorf("failed to parse HTML: %w", err)
	}

	var extractText func(*html.Node) string
	extractText = func(n *html.Node) string {
		if n.Type == html.TextNode {
			return n.Data
		}
		if n.Type == html.ElementNode && (n.Data == "script" || n.Data == "style") {
			return ""
		}
		var result string
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			text := extractText(c)
			if n.Type == html.ElementNode && (n.Data == "br" || n.Data == "p" || n.Data == "div" || strings.HasPrefix(n.Data, "h")) {
				result += "\n"
			}
			result += text
		}
		return result
	}

	textContent := extractText(doc)

	// Clean up whitespace while preserving newlines
	textContent = regexp.MustCompile(`[ \t]+`).ReplaceAllString(textContent, " ")
	textContent = regexp.MustCompile(`\n\s+`).ReplaceAllString(textContent, "\n")
	textContent = regexp.MustCompile(`\n{3,}`).ReplaceAllString(textContent, "\n\n")

	return strings.TrimSpace(textContent), nil
}

func buildSlug(spaceName string, anchestors []*confluence.Content, pageTitle string) string {
	// all but the first anchestor
	for i := 1; i < len(anchestors); i++ {
		pageTitle = anchestors[i].Title + "/" + pageTitle
	}

	return spaceName + "/" + pageTitle
}
