package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"searccher/web/crop"
	"shared/doc"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/meilisearch/meilisearch-go"
)

const DEFAULT_SNIPPET_LENGTH = 500
const MAX_SNIPPET_LENGTH = 1_000_000
const DEFAULT_MAX_PARAGRAPHS = 3
const MAX_PARAGRAPHS_LENGTH = 100

func sortInfo(sort string) (modified []string, isRelevant bool) {
	switch sort {
	case "newest":
		return []string{"lastModified:desc"}, false
	case "oldest":
		return []string{"lastModified:asc"}, false
	default: // == "relevance"
		return nil, true
	}
}

type searchRequest struct {
	Query  string `json:"query"`
	Limit  int    `json:"limit"`
	Offset int    `json:"offset"`
	Sort   string `json:"sort"`

	IgnoreEmptyFiles bool   `json:"ignoreEmptyFiles"`
	FileTypesOption  string `json:"fileTypes"`
	SinceYear        int    `json:"sinceYear"`

	AIRatio float64 `json:"aiRatio"`

	SnippetLength int `json:"snippetLength"`
	MaxParagraphs int `json:"maxParagraphs"`
}

func (s *Server) buildFilter(req searchRequest, permissionGroups []string) (filters []string) {
	if len(permissionGroups) == 0 {
		panic("permissionGroups is empty")
	}

	tags := s.Config.GetGroupsTags(permissionGroups)

	quotedTags := make([]string, 0, len(tags))
	seen := make(map[string]struct{})
	for _, tag := range tags {
		if _, ok := seen[tag]; ok {
			continue
		}
		quotedTags = append(quotedTags, fmt.Sprintf("%q", tag))
		seen[tag] = struct{}{}
	}

	filters = append(filters, fmt.Sprintf("permissionTag IN [%s]", strings.Join(quotedTags, ",")))

	if req.IgnoreEmptyFiles {
		filters = append(filters, "contentSize > 0")
	}

	switch req.FileTypesOption {
	case "only-code":
		filters = append(filters, "isCode = true")
	case "ignore-code":
		filters = append(filters, "isCode = false")
	default:
		// Keep both code and non-code files
	}

	if req.SinceYear > 0 {
		// LastModified is a unix timestamp in seconds, so we need to convert the year to a timestamp
		ts := time.Date(req.SinceYear, time.January, 1, 0, 0, 0, 0, time.UTC).Unix()
		filters = append(filters, fmt.Sprintf("lastModified >= %d", ts))
	}

	return filters
}

func (s *Server) Search(c *fiber.Ctx) (err error) {
	var req searchRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).SendString("invalid request body")
	}

	var permissionGroups []string
	// if api key present - secure string compare
	if _, perm, err := s.Config.PermissionsForToken(c.Query("apiKey")); err == nil {
		// Use API key to get permission groups
		permissionGroups = perm
	} else {
		userInfo, err := s.UserInfo(c)
		if err != nil {
			s.Logger.Printf("search: UserInfo failed: %v", err)
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": err.Error()})
		}
		if len(userInfo.PermissionGroups) == 0 {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
				"message":     "You are not permitted to view any files. Ask an administrator for access.",
				"group_error": true,
			})
		}
		permissionGroups = userInfo.PermissionGroups
	}

	ctx, cancel := context.WithTimeout(c.Context(), 2*time.Minute)
	defer cancel()

	if req.Limit == 0 {
		req.Limit = 25
	}
	if req.Offset < 0 {
		req.Offset = 0
	}
	if req.SnippetLength <= 0 || req.SnippetLength > MAX_SNIPPET_LENGTH {
		req.SnippetLength = DEFAULT_SNIPPET_LENGTH
	}
	if req.MaxParagraphs <= 0 || req.MaxParagraphs > MAX_PARAGRAPHS_LENGTH {
		req.MaxParagraphs = DEFAULT_MAX_PARAGRAPHS
	}
	if req.Sort != "relevance" || req.AIRatio < 0 || req.AIRatio > 1 {
		req.AIRatio = 0
	}

	isSimilarQuery := strings.HasPrefix(req.Query, "similar:")
	if (req.AIRatio > 0 || isSimilarQuery) && s.Embedder == nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "AI search is not enabled",
		})
	}

	// If query starts with "similar:", use the similarity functionality.
	if isSimilarQuery {
		docID := strings.TrimSpace(strings.TrimPrefix(req.Query, "similar:"))

		var d *SearchResult
		if req.Offset == 0 {
			d = new(SearchResult)
			err = s.Index.GetDocument(docID, &meilisearch.DocumentQuery{
				Fields: []string{"id", "url", "inFolderUrl", "content", "slug", "title", "lastModified", "isCode", "permissionTag"},
			}, d)
			if err != nil {
				if meiliErr, ok := err.(*meilisearch.Error); ok && meiliErr.StatusCode == 404 {
					return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
						"error": "document not found",
					})
				}

				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
					"error": "search backend error",
				})
			}
			if !s.Config.UserCanAccessTag(permissionGroups, d.PermissionTag) {
				return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
					"error": "document not found",
				})
			}
		}

		var res meilisearch.SimilarDocumentResult
		err := s.Index.SearchSimilarDocumentsWithContext(ctx, &meilisearch.SimilarDocumentQuery{
			Embedder:              s.Embedder.EmbedModel,
			Id:                    docID,
			Filter:                "(" + strings.Join(s.buildFilter(req, permissionGroups), ") AND (") + ")",
			Offset:                int64(req.Offset),
			Limit:                 int64(req.Limit),
			ShowRankingScore:      true,
			RankingScoreThreshold: 0.1,
		}, &res)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": "similarity search backend error",
			})
		}

		hits := s.processHits(res.Hits, req.Query, req.SnippetLength, req.MaxParagraphs)
		if d != nil {
			d.Content = crop.CropRelevantContent(d.Content, req.Query, req.SnippetLength*req.MaxParagraphs, 1, s.Config.Synonyms.Merged)
			hits = append([]SearchResult{*d}, hits...)
		}
		return c.JSON(SearchResponse{
			Hits:               hits,
			EstimatedTotalHits: res.EstimatedTotalHits,
			Limit:              res.Limit,
			ProcessingTimeMS:   res.ProcessingTimeMS,
			Query:              req.Query,
		})
	}

	var (
		hybrid         *meilisearch.SearchRequestHybrid
		embedVector    []float32
		scoreThreshold float64
	)
	if req.AIRatio > 0 {
		embedVector, err = s.Embedder.EmbedText(req.Query, true, c.Context())
		if err != nil {
			s.Logger.Printf("Error embedding text: %v", err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": "error while embedding user query",
			})
		}

		hybrid = &meilisearch.SearchRequestHybrid{
			SemanticRatio: req.AIRatio,
			Embedder:      s.Embedder.EmbedModel,
		}

		scoreThreshold = 0.1
	}

	meiliSort, isRelevant := sortInfo(req.Sort)

	// If we're sorting by oldest/newest, ensure *all* words need to be present,
	// otherwise we get a lot of results with irrelevant content.
	matchingStrategy := meilisearch.Frequency
	if !isRelevant {
		matchingStrategy = meilisearch.All
	}
	searchResponse, err := s.Index.SearchWithContext(ctx, req.Query, &meilisearch.SearchRequest{
		Limit:                 int64(req.Limit),
		Offset:                int64(req.Offset),
		AttributesToHighlight: []string{"title", "slug", "content"},
		AttributesToRetrieve:  []string{"id", "url", "inFolderUrl", "content", "slug", "title", "lastModified", "isCode"},
		Sort:                  meiliSort,
		Filter:                s.buildFilter(req, permissionGroups),
		HighlightPreTag:       "<mark>",
		ShowRankingScore:      true,
		HighlightPostTag:      "</mark>",
		RankingScoreThreshold: scoreThreshold,
		Hybrid:                hybrid,
		Vector:                embedVector,
		MatchingStrategy:      matchingStrategy,
	})
	if err != nil {
		if !(errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "context canceled")) {
			s.Logger.Printf("Error searching meilisearch: %v", err)
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "normal search backend error",
		})
	}

	start := time.Now()
	hits := s.processHits(searchResponse.Hits, req.Query, req.SnippetLength, req.MaxParagraphs)
	t := time.Since(start)
	if t > 100*time.Millisecond {
		s.Logger.Printf("Processing search hits took %s for query %q", t, req.Query)
	}
	return c.JSON(SearchResponse{
		Hits:               hits,
		EstimatedTotalHits: searchResponse.EstimatedTotalHits,
		Limit:              searchResponse.Limit,
		ProcessingTimeMS:   searchResponse.ProcessingTimeMs,
		Query:              req.Query,
	})
}

type SearchResponse struct {
	Hits               []SearchResult `json:"hits"`
	EstimatedTotalHits int64          `json:"estimatedTotalHits"`
	Limit              int64          `json:"limit"`
	ProcessingTimeMS   int64          `json:"processingTimeMs"`
	Query              string         `json:"query"`
}

type SearchResult struct {
	RankingScore float64 `json:"_rankingScore"`

	ID           string           `json:"id"`
	Content      string           `json:"content"`
	InFolderURL  string           `json:"inFolderUrl,omitempty"`
	IsCode       bool             `json:"isCode"`
	LastModified doc.FloatyNumber `json:"lastModified"`
	Slug         string           `json:"slug"`
	Title        string           `json:"title"`
	URL          string           `json:"url"`

	PermissionTag string `json:"permissionTag,omitempty"`
}

func (s *Server) processHits(hits meilisearch.Hits, query string, snippetLength int, maxParagraphs int) (result []SearchResult) {
	result = make([]SearchResult, 0, len(hits))
	for _, hit := range hits {
		// Check if there's a formatted version first
		var data map[string]json.RawMessage = hit
		if formattedRaw, exists := hit["_formatted"]; exists {
			var formatted map[string]json.RawMessage
			if err := json.Unmarshal(formattedRaw, &formatted); err == nil {
				data = formatted
			}
		}

		var sr SearchResult

		// Parse ID
		if idRaw, exists := data["id"]; exists {
			if err := json.Unmarshal(idRaw, &sr.ID); err != nil {
				panic("hit does not contain valid id")
			}
		} else {
			panic("hit does not contain id")
		}

		// Parse Content
		if contentRaw, exists := data["content"]; exists {
			if err := json.Unmarshal(contentRaw, &sr.Content); err != nil {
				panic("hit does not contain valid content")
			}
		} else {
			panic("hit does not contain content")
		}
		sr.Content = crop.CropRelevantContent(sr.Content, query, snippetLength, maxParagraphs, s.Config.Synonyms.Merged)

		// Parse InFolderURL (optional)
		if inFolderURLRaw, exists := data["inFolderUrl"]; exists {
			json.Unmarshal(inFolderURLRaw, &sr.InFolderURL) // Ignore error for optional field
		}

		// Parse IsCode
		if isCodeRaw, exists := data["isCode"]; exists {
			if err := json.Unmarshal(isCodeRaw, &sr.IsCode); err != nil {
				panic("hit does not contain valid isCode")
			}
		} else {
			panic("hit does not contain isCode")
		}

		// Parse LastModified (can be float64 or string)
		if lastModifiedRaw, exists := data["lastModified"]; exists {
			var lastModified float64
			// Try parsing as float64 first
			if err := json.Unmarshal(lastModifiedRaw, &lastModified); err != nil {
				// If that fails, try parsing as string
				var lastModifiedStr string
				if err := json.Unmarshal(lastModifiedRaw, &lastModifiedStr); err != nil {
					panic("hit does not contain valid lastModified")
				}
				if _, err := fmt.Sscanf(lastModifiedStr, "%f", &lastModified); err != nil {
					panic(fmt.Sprintf("hit does not contain valid lastModified: %v", err))
				}
			}
			sr.LastModified = doc.FloatyNumber(lastModified)
		} else {
			panic("hit does not contain lastModified")
		}

		// Parse Slug
		if slugRaw, exists := data["slug"]; exists {
			if err := json.Unmarshal(slugRaw, &sr.Slug); err != nil {
				panic("hit does not contain valid slug")
			}
		} else {
			panic("hit does not contain slug")
		}

		// Parse Title
		if titleRaw, exists := data["title"]; exists {
			if err := json.Unmarshal(titleRaw, &sr.Title); err != nil {
				panic("hit does not contain valid title")
			}
		} else {
			panic("hit does not contain title")
		}

		// Parse URL
		if urlRaw, exists := data["url"]; exists {
			if err := json.Unmarshal(urlRaw, &sr.URL); err != nil {
				panic("hit does not contain valid url")
			}
		} else {
			panic("hit does not contain url")
		}

		// Parse RankingScore (optional)
		if scoreRaw, exists := data["_rankingScore"]; exists {
			json.Unmarshal(scoreRaw, &sr.RankingScore) // Ignore error, default to 0
		}

		result = append(result, sr)
	}
	return result
}
