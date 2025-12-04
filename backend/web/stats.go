package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"shared/doc"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/meilisearch/meilisearch-go"
)

func (s *Server) IndexStats(c *fiber.Ctx) error {
	stats, err := s.Index.GetStats()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).SendString("failed to get stats")
	}

	return c.JSON(fiber.Map{
		"stats":      stats,
		"ai_enabled": s.Embedder != nil,
	})
}

func (s *Server) DocumentStats(c *fiber.Ctx) error {
	user, err := s.UserInfo(c)
	if err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": err.Error()})
	}

	if !user.IsAdmin {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "You are not an admin"})
	}

	stats, err := s.CalculateDocumentStatistics(c.Context(), s.Index, 100)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).SendString("failed to get percentiles")
	}

	return c.JSON(stats)
}

type DocumentStats struct {
	TotalDocuments   int
	TotalContentSize int64
	AverageSize      float64

	// Size-based
	LargestDocs      []DocumentInfo
	SizeDistribution map[string]int

	// Extension-based (from URL)
	extensionStats map[string]CountInfo
	ExtensionStats []struct {
		Extension   string  `json:"extension"`
		Count       int     `json:"count"`
		TotalSize   int64   `json:"totalSize"`
		AverageSize float64 `json:"averageSize"`
	}

	// Extractor-based
	ExtractorStats map[string]CountInfo

	// Permission-based
	PermissionStats map[string]int

	// Code vs non-code
	CodeStats map[string]int

	// Modification date-based
	ModificationDateStats ModStats `json:"modificationDateStats"`
}

type ModStats struct {
	StartDate time.Time `json:"startDate"`
	EndDate   time.Time `json:"endDate"`
	Counts    []int     `json:"counts"` // Daily counts starting from StartDate
}

type DocumentInfo struct {
	ID            string
	Title         string
	Slug          string
	ContentSize   int64
	Extension     string
	Extractor     string
	PermissionTag string
	IsCode        bool
}

type CountInfo struct {
	Count       int
	TotalSize   int64
	AverageSize float64
}

// Subset of Document struct with only fields we need for statistics
type StatDocument struct {
	ID            string `json:"id"`
	Slug          string `json:"slug"`
	Title         string `json:"title"`
	ContentSize   int64  `json:"contentSize"`
	IsCode        bool   `json:"isCode"`
	Extractor     string `json:"extractor"`
	PermissionTag string `json:"permissionTag"`
	// Unix seconds
	LastModified doc.FloatyNumber `json:"lastModified"`
}

func (s *Server) CalculateDocumentStatistics(ctx context.Context, index meilisearch.IndexManager, maxResults int) (*DocumentStats, error) {
	stats := &DocumentStats{
		SizeDistribution: make(map[string]int),
		extensionStats:   make(map[string]CountInfo),
		ExtractorStats:   make(map[string]CountInfo),
		PermissionStats:  make(map[string]int),
		CodeStats:        make(map[string]int),
		LargestDocs:      make([]DocumentInfo, 0, maxResults),
	}

	var offset int64
	const batchSize = 1000
	modDateCounts := make(map[time.Time]int)
	firstDate := time.Now().Truncate(24 * time.Hour)
	lastDate := firstDate

	for {
		var res meilisearch.DocumentsResult

		err := index.GetDocumentsWithContext(ctx, &meilisearch.DocumentsQuery{
			Offset: offset,
			Limit:  batchSize,
			Fields: []string{"id", "slug", "title", "contentSize", "isCode", "extractor", "permissionTag", "lastModified"},
		}, &res)

		if err != nil {
			if errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "context canceled") {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("failed to fetch documents: %w", err)
		}

		// Process the batch
		for _, hit := range res.Results {
			doc, err := parseDocument(hit)
			if err != nil {
				s.Logger.Printf("[Stats] Skipping invalid document: %v", err)
				continue // Skip invalid documents
			}

			// Update basic stats
			stats.TotalDocuments++
			stats.TotalContentSize += int64(doc.ContentSize)

			// Create DocumentInfo for top lists
			docInfo := DocumentInfo{
				ID:            doc.ID,
				Title:         doc.Title,
				Slug:          doc.Slug,
				ContentSize:   doc.ContentSize,
				Extension:     extractExtension(doc.Slug),
				Extractor:     doc.Extractor,
				PermissionTag: doc.PermissionTag,
				IsCode:        doc.IsCode,
			}

			// Size distribution
			sizeCategory := categorizeSizeByBytes(int64(doc.ContentSize))
			stats.SizeDistribution[sizeCategory]++

			// Extension stats
			ext := docInfo.Extension
			extInfo := stats.extensionStats[ext]
			extInfo.Count++
			extInfo.TotalSize += int64(doc.ContentSize)
			extInfo.AverageSize = float64(extInfo.TotalSize) / float64(extInfo.Count)
			stats.extensionStats[ext] = extInfo

			// Extractor stats
			extractorInfo := stats.ExtractorStats[doc.Extractor]
			extractorInfo.Count++
			extractorInfo.TotalSize += int64(doc.ContentSize)
			extractorInfo.AverageSize = float64(extractorInfo.TotalSize) / float64(extractorInfo.Count)
			stats.ExtractorStats[doc.Extractor] = extractorInfo

			// Permission stats
			stats.PermissionStats[doc.PermissionTag]++

			// Modification date stats
			modificationDay := time.Unix(int64(doc.LastModified), 0).Truncate(24 * time.Hour)
			if _, exists := modDateCounts[modificationDay]; !exists {
				modDateCounts[modificationDay] = 1
				if modificationDay.After(lastDate) {
					lastDate = modificationDay
				}
				if modificationDay.Before(firstDate) {
					firstDate = modificationDay
				}
			} else {
				modDateCounts[modificationDay]++
			}

			// Code vs non-code stats
			if doc.IsCode {
				stats.CodeStats["code"]++
			} else {
				stats.CodeStats["non-code"]++
			}

			// Update top list
			updateTopList(&stats.LargestDocs, docInfo, maxResults, func(a, b DocumentInfo) bool {
				return a.ContentSize > b.ContentSize
			})
		}

		// Check if we've processed all documents
		if len(res.Results) < batchSize {
			break
		}

		offset += int64(len(res.Results))

		// Check for context cancellation
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
	}

	// Calculate average size
	if stats.TotalDocuments > 0 {
		stats.AverageSize = float64(stats.TotalContentSize) / float64(stats.TotalDocuments)
	}

	// Remove extension stats where count < 5 and make sure it's less than maxResults
	for ext, info := range stats.extensionStats {
		if info.Count < 5 {
			delete(stats.extensionStats, ext)
		}
	}
	// Convert extension stats to slice for easier JSON serialization
	for ext, info := range stats.extensionStats {
		stats.ExtensionStats = append(stats.ExtensionStats, struct {
			Extension   string  `json:"extension"`
			Count       int     `json:"count"`
			TotalSize   int64   `json:"totalSize"`
			AverageSize float64 `json:"averageSize"`
		}{
			Extension:   ext,
			Count:       info.Count,
			TotalSize:   info.TotalSize,
			AverageSize: info.AverageSize,
		})
	}
	// Sort extension stats by count descending
	sort.Slice(stats.ExtensionStats, func(i, j int) bool {
		return stats.ExtensionStats[i].Count > stats.ExtensionStats[j].Count
	})
	// Limit to maxResults
	if len(stats.ExtensionStats) > maxResults {
		stats.ExtensionStats = stats.ExtensionStats[:maxResults]
	}

	var mods = ModStats{
		StartDate: firstDate,
		EndDate:   lastDate,
		Counts:    make([]int, 0, len(modDateCounts)),
	}
	// Fill in the counts for each day
	for date := firstDate; !date.After(lastDate); date = date.AddDate(0, 0, 1) {
		count, exists := modDateCounts[date]
		if !exists {
			count = 0
		}
		mods.Counts = append(mods.Counts, count)
	}
	stats.ModificationDateStats = mods

	return stats, nil
}

func parseDocument(hit meilisearch.Hit) (*StatDocument, error) {
	sdoc := &StatDocument{}

	// Parse ID
	if idRaw, exists := hit["id"]; exists {
		if err := json.Unmarshal(idRaw, &sdoc.ID); err != nil {
			return nil, fmt.Errorf("invalid id: %w", err)
		}
	} else {
		return nil, fmt.Errorf("missing id")
	}

	// Parse Slug
	if slugRaw, exists := hit["slug"]; exists {
		if err := json.Unmarshal(slugRaw, &sdoc.Slug); err != nil {
			return nil, fmt.Errorf("invalid slug: %w", err)
		}
	} else {
		return nil, fmt.Errorf("missing slug")
	}

	// Parse Title (optional)
	if titleRaw, exists := hit["title"]; exists {
		json.Unmarshal(titleRaw, &sdoc.Title) // Ignore error, default to empty string
	}

	// Parse ContentSize
	if contentSizeRaw, exists := hit["contentSize"]; exists {
		var contentSize float64
		if err := json.Unmarshal(contentSizeRaw, &contentSize); err != nil {
			return nil, fmt.Errorf("invalid contentSize: %w", err)
		}
		sdoc.ContentSize = int64(contentSize)
	} else {
		return nil, fmt.Errorf("missing contentSize")
	}

	// Parse IsCode (optional, default to false)
	if isCodeRaw, exists := hit["isCode"]; exists {
		json.Unmarshal(isCodeRaw, &sdoc.IsCode) // Ignore error, default to false
	}

	// Parse Extractor (optional, default to "unknown")
	if extractorRaw, exists := hit["extractor"]; exists {
		if err := json.Unmarshal(extractorRaw, &sdoc.Extractor); err != nil {
			sdoc.Extractor = "unknown"
		}
	} else {
		sdoc.Extractor = "unknown"
	}

	// Parse PermissionTag (optional, default to "unknown")
	if permissionTagRaw, exists := hit["permissionTag"]; exists {
		if err := json.Unmarshal(permissionTagRaw, &sdoc.PermissionTag); err != nil {
			sdoc.PermissionTag = "unknown"
		}
	} else {
		sdoc.PermissionTag = "unknown"
	}

	// Parse LastModified (can be string or float64)
	if lastModifiedRaw, exists := hit["lastModified"]; exists {
		// Try parsing as string first
		var modifiedAtStr string
		if err := json.Unmarshal(lastModifiedRaw, &modifiedAtStr); err == nil {
			modAt, err := strconv.ParseInt(modifiedAtStr, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid lastModified string format: %w", err)
			}
			sdoc.LastModified = doc.FloatyNumber(modAt)
		} else {
			// Try parsing as float64
			var modifiedAtFloat float64
			if err := json.Unmarshal(lastModifiedRaw, &modifiedAtFloat); err != nil {
				return nil, fmt.Errorf("invalid lastModified format: %w", err)
			}
			sdoc.LastModified = doc.FloatyNumber(modifiedAtFloat)
		}
	} else {
		return nil, fmt.Errorf("missing lastModified")
	}

	return sdoc, nil
}

func extractExtension(url string) string {
	if url == "" {
		return "unknown"
	}
	parts := strings.Split(url, "/")
	if len(parts) == 0 {
		return "unknown"
	}
	filename := parts[len(parts)-1]
	// split off ? and # if they exist
	if idx := strings.IndexAny(filename, "?#"); idx != -1 {
		filename = filename[:idx]
	}
	if idx := strings.LastIndex(filename, "."); idx != -1 && idx < len(filename)-1 {
		return strings.ToLower(filename[idx+1:])
	}
	return "unknown"
}

func updateTopList(list *[]DocumentInfo, doc DocumentInfo, maxSize int, compare func(a, b DocumentInfo) bool) {
	if len(*list) < maxSize {
		*list = append(*list, doc)
		sort.Slice(*list, func(i, j int) bool {
			return compare((*list)[i], (*list)[j])
		})
	} else if compare(doc, (*list)[maxSize-1]) {
		(*list)[maxSize-1] = doc
		sort.Slice(*list, func(i, j int) bool {
			return compare((*list)[i], (*list)[j])
		})
	}

	if len(*list) > maxSize {
		*list = (*list)[:maxSize]
	}
}

func categorizeSizeByBytes(size int64) string {
	if size == 0 {
		return "empty (0B)"
	} else if size < 1024 {
		return "tiny (<1KB)"
	} else if size < 1024*100 {
		return "small (1-100KB)"
	} else if size < 1024*1024 {
		return "medium (100KB-1MB)"
	} else if size < 10*1024*1024 {
		return "large (1-10MB)"
	} else {
		return "huge (>10MB)"
	}
}
