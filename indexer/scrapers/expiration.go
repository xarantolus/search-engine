package scrapers

import (
	"context"
	"errors"
	"fmt"
	"log"
	"shared/doc"
	"shared/embedding"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/dustin/go-humanize"
	"github.com/meilisearch/meilisearch-go"
)

type BasicDocument struct {
	LastModified         doc.FloatyNumber
	Extractor            string
	PermissionTag        string
	IndexTime            doc.FloatyNumber
	CurrentEmbeddingsLen int
}

// MemorySize returns the byte size of the struct including all fields
func (b *BasicDocument) MemorySize() int {
	return int(unsafe.Sizeof(*b) + unsafe.Sizeof(b.LastModified) + unsafe.Sizeof(b.Extractor) + uintptr(len(b.Extractor)) + unsafe.Sizeof(b.PermissionTag) + uintptr(len(b.PermissionTag)) + unsafe.Sizeof(b.IndexTime) + unsafe.Sizeof(b.CurrentEmbeddingsLen))
}

type ExpirationChecker struct {
	log   *log.Logger
	index meilisearch.IndexManager

	rwLock  sync.RWMutex
	entries map[string]BasicDocument

	embed *embedding.EmbedConfig

	reindexAfter time.Duration
}

func NewExpirationChecker(ctx context.Context, embeddingModel *embedding.EmbedConfig, log *log.Logger, index meilisearch.IndexManager, reindexAfter time.Duration, preCache bool) *ExpirationChecker {
	ex := &ExpirationChecker{log: log, index: index, entries: make(map[string]BasicDocument), reindexAfter: reindexAfter, embed: embeddingModel}
	if preCache {
		go ex.updatePeriodically(ctx)
	}

	return ex
}

func (e *ExpirationChecker) updatePeriodically(ctx context.Context) {
	e.loadAll(ctx)

	var t = time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.log.Printf("Starting cache reload")
			e.loadAll(ctx)
		}
	}
}

func (e *ExpirationChecker) loadAll(ctx context.Context) {
	var offset uint64
	const increase = 1000
	start := time.Now()

	var (
		res           meilisearch.DocumentsResult
		fetchAttempts int
		cacheSize     uint64
	)

	for {
		err := e.index.GetDocumentsWithContext(ctx, &meilisearch.DocumentsQuery{
			Offset:          int64(offset),
			Limit:           increase,
			Fields:          []string{"id", "lastModified", "version", "extractor", "permissionTag", "indexTime"},
			RetrieveVectors: e.embed != nil,
		}, &res)
		if err != nil {
			fetchAttempts++
			if errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "context canceled") {
				return
			}

			if fetchAttempts < 3 {
				e.log.Printf("Error while loading cache: %v", err)
				time.Sleep(time.Second)
				continue
			}
			e.log.Printf("Stopping cache load retry after %d attempts: %v", fetchAttempts, err)
			return
		}

		fetchAttempts = 0

		_ = e.BulkAdd(res.Results, func(d meilisearch.Hit) (string, *BasicDocument) {
			type DocumentHit struct {
				ID            string                 `json:"id"`
				LastModified  float64                `json:"lastModified"`
				Version       float64                `json:"version"`
				IndexTime     float64                `json:"indexTime"`
				Extractor     string                 `json:"extractor"`
				PermissionTag string                 `json:"permissionTag"`
				Vectors       map[string]interface{} `json:"_vectors,omitempty"`
			}

			var docHit DocumentHit
			if err := d.DecodeInto(&docHit); err != nil {
				return "", nil
			}

			if int(docHit.Version) != doc.DocumentVersion {
				return "", nil
			}

			var embeddingsLen int
			if e.embed != nil && docHit.Vectors != nil {
				if embeds, ok := docHit.Vectors[e.embed.EmbedModel].(map[string]interface{}); ok {
					if embeddings, ok := embeds["embeddings"].([]interface{}); ok && len(embeddings) > 0 {
						if firstEmbedding, ok := embeddings[0].([]interface{}); ok {
							embeddingsLen = len(firstEmbedding)
						}
					}
				}
			}

			doc := BasicDocument{
				LastModified:         doc.FloatyNumber(int64(docHit.LastModified)),
				Extractor:            docHit.Extractor,
				PermissionTag:        docHit.PermissionTag,
				IndexTime:            doc.FloatyNumber(int64(docHit.IndexTime)),
				CurrentEmbeddingsLen: embeddingsLen,
			}
			cacheSize += uint64(doc.MemorySize())
			return docHit.ID, &doc
		})

		select {
		case <-ctx.Done():
			return
		default:
		}

		if len(res.Results) < increase {
			break
		}

		offset += increase
	}

	e.log.Printf("Finished loading cache in %s: %d entries available for fast lookups (%s)", time.Since(start).Round(time.Second), len(e.entries), humanize.Bytes(uint64(cacheSize)))
}

func (e *ExpirationChecker) BulkAdd(list []meilisearch.Hit, transform func(meilisearch.Hit) (string, *BasicDocument)) int {
	e.rwLock.Lock()
	defer e.rwLock.Unlock()

	var c int
	for _, item := range list {
		key, doc := transform(item)
		if len(key) == 0 || doc == nil {
			continue
		}
		e.entries[key] = *doc
		c++
	}

	return c
}

const (
	ReasonCached                = "cached"
	ReasonNotFound              = "not_found"
	ReasonError                 = "error"
	ReasonModified              = "modified"
	ReasonVersionMismatch       = "doc_version_mismatch"
	ReasonReIndex               = "last_extract_failed"
	ReasonExtractorMismatch     = "extractor_mismatch"
	ReasonPermissionTagMismatch = "permission_tag_mismatch"
	ReasonReindexAfterExceeded  = "document_expired"
	ReasonEmbeddingsMismatch    = "embeddings_mismatch"
)

func (e *ExpirationChecker) IsDocUpToDate(uid string, modTime int64, extractor string, perm string) (exists bool, reason string, err error) {
	e.rwLock.RLock()
	basicDoc, found := e.entries[uid]
	e.rwLock.RUnlock()

	if found {
		lastIndexTime := time.Unix(int64(basicDoc.IndexTime), 0)

		if int64(basicDoc.LastModified) == modTime && modTime != 0 &&
			extractor == basicDoc.Extractor && basicDoc.PermissionTag == perm &&
			time.Since(lastIndexTime) < e.reindexAfter {
			if e.embed == nil {
				return true, ReasonCached, nil
			} else if basicDoc.CurrentEmbeddingsLen == e.embed.Dimensions {
				return true, ReasonCached, nil
			}
		}
	}

	var document doc.Document
	err = e.index.GetDocument(uid, &meilisearch.DocumentQuery{
		Fields:          []string{"lastModified", "version", "extractor", "permissionTag", "indexTime"},
		RetrieveVectors: e.embed != nil,
	}, &document)
	if err != nil {
		var meiliErr *meilisearch.Error
		if errors.As(err, &meiliErr) && meiliErr.StatusCode == 404 {
			return false, ReasonNotFound, err
		}
		log.Printf("Error while fetching document %s: %v", uid, err)
		return false, ReasonError, err
	}

	if modTime == 0 {
		return false, fmt.Sprintf("%s: modTime is 0", ReasonModified), nil
	}
	if int64(document.LastModified) != modTime {
		return false, fmt.Sprintf("%s: current=%d, new=%d", ReasonModified, document.LastModified, modTime), nil
	}

	if document.Version != uint32(doc.DocumentVersion) {
		return false, fmt.Sprintf("%s: current=%d, new=%d", ReasonVersionMismatch, document.Version, doc.DocumentVersion), nil
	}

	if document.ReIndex {
		return false, ReasonReIndex, nil
	}

	if extractor != document.Extractor {
		return false, fmt.Sprintf("%s: current=%q, expected=%q", ReasonExtractorMismatch, document.Extractor, extractor), nil
	}

	if document.PermissionTag != perm {
		return false, fmt.Sprintf("%s: current=%q, new=%q", ReasonPermissionTagMismatch, document.PermissionTag, perm), nil
	}

	lastIndexTime := time.Unix(int64(document.IndexTime), 0)
	if time.Since(lastIndexTime) >= e.reindexAfter {
		return false, ReasonReindexAfterExceeded, nil
	}

	if e.embed == nil {
		return true, ReasonCached, nil
	}

	if len(document.EmbedVectors) == 0 {
		return false, fmt.Sprintf("%s: missing embeddings", ReasonEmbeddingsMismatch), nil
	}

	modelEmbeds, exists := document.EmbedVectors[e.embed.EmbedModel]
	if !exists || modelEmbeds == nil || len(modelEmbeds.Embeddings) == 0 {
		return false, fmt.Sprintf("%s: no valid embeddings for model %s", ReasonEmbeddingsMismatch, e.embed.EmbedModel), nil
	}

	firstEmbed := modelEmbeds.Embeddings[0]
	if firstEmbed == nil || len(firstEmbed) != e.embed.Dimensions {
		return false, fmt.Sprintf("%s: dimension mismatch (expected %d)", ReasonEmbeddingsMismatch, e.embed.Dimensions), nil
	}

	return true, ReasonCached, nil
}
