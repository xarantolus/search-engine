package doc

import (
	"fmt"
	"log"
	"shared/embedding"
	"strconv"
	"sync"
	"time"
	"unsafe"

	"github.com/dustin/go-humanize"
	"github.com/meilisearch/meilisearch-go"
)

// Increase version if the structure of the document changed.
// Documents will be reindexed because IsDocUpToDate will return false.
const DocumentVersion = 6

type Document struct {
	// A unique ID calculated by hashURL
	ID string `json:"id"`
	// URL to the document
	URL string `json:"url"`
	// Optional URL to a link where the document is shown in a folder/UI, without directly opening it.
	InFolderURL string `json:"inFolderUrl,omitempty"`
	// Extracted text content of the document
	Content string `json:"content"`
	// ContentSize is the size of the content in bytes
	ContentSize int64 `json:"contentSize"`
	// Slug is a human-readable identifier for the document
	Slug string `json:"slug"`
	// Title of the document
	Title string `json:"title"`
	// Last modified time of the document (unix seconds) - as in the actual file, not when it was indexed
	LastModified FloatyNumber `json:"lastModified"`
	// Version of the document structure - used to check if the document needs to be reindexed
	Version uint32 `json:"version"`
	// Whether the document needs to be reindexed.
	// This is set to true when extracting text or embeddings failed - we should try again on the next run.
	ReIndex bool `json:"reindex"`
	// Whether the document is considered code. Not all files in a git repo are considered code (e.g. Markdown files are not)
	IsCode bool `json:"isCode"`
	// Which extractor was used to extract the content - this allows us to change the extractor and only reindex affected documents
	Extractor string `json:"extractor"`
	// The following fields is used for filtering permissions.
	// Basically, a user gets a list of permissions, and we filter out documents that don't match any of the permissions.
	PermissionTag string `json:"permissionTag"`
	// This is the Unix seconds time when the document was last indexed.
	// If this is older than a couple of days, we should reindex the document.
	// We can then use this to delete documents that were deleted in the source.
	IndexTime FloatyNumber `json:"indexTime"`

	// Embeddings for the document
	EmbedVectors map[string]*StoredVectors `json:"_vectors,omitempty"`
}

func (d *Document) embeddingText() string {
	return fmt.Sprintf("%s\n%s\n%s", d.Title, d.Slug, d.Content)
}

type StoredVectors struct {
	Embeddings [][]float32 `json:"embeddings"`
	Regenerate bool        `json:"regenerate"`
}

// This is a workaround for JSON encoding/decoding for int64 in Go outputs a string
type FloatyNumber int64

func (f FloatyNumber) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatInt(int64(f), 10)), nil
}

func (f *FloatyNumber) UnmarshalJSON(data []byte) error {
	// If there are quotes, remove them
	if len(data) >= 2 && data[0] == '"' && data[len(data)-1] == '"' {
		data = data[1 : len(data)-1]
	}
	i, err := strconv.ParseInt(string(data), 10, 64)
	if err != nil {
		return err
	}
	*f = FloatyNumber(i)
	return nil
}

func (d *Document) ApproximateMemorySize() int {
	return len(d.ID) + len(d.URL) + len(d.Content) + len(d.Slug) + len(d.Title) + int(unsafe.Sizeof(Document{})) + int(unsafe.Sizeof(StoredVectors{}))
}

// Max document size is 100 MB, so we crop it to ~85 MB
const MaxContentLength = 85 * 1024 * 1024

func (d *Document) TruncateContent(size uint) {
	if uint(len(d.Content)) > size {
		d.Content = d.Content[:size]
		d.ContentSize = int64(len(d.Content))
	}
}

type DocumentBatcher struct {
	lock      sync.Mutex
	documents []Document

	logger *log.Logger

	Index      meilisearch.IndexManager
	embedder   *embedding.EmbedConfig
	lastTaskID *int64

	MaxCommitSize     int
	MaxDocumentsCount int

	MaxDocumentSize int

	currentSizeBytes int
}

// Meilisearch has a limit of 100MB per commit, + there's some JSON overhead
const MEILISEARCH_SAFE_MAX_COMMIT_SIZE = 75 * 1024 * 1024

func NewDocumentBatcher(index meilisearch.IndexManager, embedder *embedding.EmbedConfig, maxDocSize int, maxSize int, maxDocs int, logger *log.Logger) *DocumentBatcher {
	return &DocumentBatcher{
		Index:             index,
		MaxCommitSize:     maxSize,
		MaxDocumentSize:   maxDocSize,
		MaxDocumentsCount: maxDocs,
		logger:            logger,
		embedder:          embedder,
	}
}

func (b *DocumentBatcher) AddDocument(d Document) (numCommitted int, err error) {
	// Ensure all necessary fields are set.
	// if something were not given here, we definitely have a programming error
	if d.ID == "" {
		panic("Document added without ID set")
	}
	if d.URL == "" {
		panic("Document added without URL set")
	}
	if d.PermissionTag == "" {
		panic("Document added without PermissionTag set")
	}
	if d.LastModified <= 0 {
		panic("Document added with LastModified set to " + strconv.FormatInt(int64(d.LastModified), 10))
	}
	if d.Version != DocumentVersion {
		panic("Document added with Version set to " + strconv.FormatUint(uint64(d.Version), 10))
	}
	d.ContentSize = int64(len(d.Content))

	if b.MaxDocumentSize > 0 {
		prev := len(d.Content)
		d.TruncateContent(uint(b.MaxDocumentSize))
		if len(d.Content) != prev {
			b.logger.Printf("Truncated content of %q to %s", d.Slug, humanize.Bytes(uint64(len(d.Content))))
		}
	}

	if b.embedder == nil {
		d.EmbedVectors = nil
	} else {
		if d.EmbedVectors == nil {
			d.EmbedVectors = make(map[string]*StoredVectors)
			d.EmbedVectors[b.embedder.EmbedModel] = nil
		}

		var previousDocument Document
		docErr := b.Index.GetDocument(d.ID, &meilisearch.DocumentQuery{
			Fields:          []string{"lastModified", "version", "extractor"},
			RetrieveVectors: true,
		}, &previousDocument)
		var haveCurrentModel bool
		if docErr == nil && len(previousDocument.EmbedVectors) > 0 {
			_, haveCurrentModel = previousDocument.EmbedVectors[b.embedder.EmbedModel]
		}
		if docErr == nil && d.LastModified == previousDocument.LastModified && d.Version == previousDocument.Version &&
			d.Extractor == previousDocument.Extractor &&
			haveCurrentModel && len(previousDocument.EmbedVectors[b.embedder.EmbedModel].Embeddings) > 0 &&
			len(previousDocument.EmbedVectors[b.embedder.EmbedModel].Embeddings[0]) == b.embedder.Dimensions {
			d.EmbedVectors = previousDocument.EmbedVectors
		} else {
			vecs, embedErr := b.embedder.EmbedText(d.embeddingText(), true)
			if embedErr != nil {
				b.logger.Printf("Failed to embed document %s: %s", d.Slug, embedErr)
				d.EmbedVectors[b.embedder.EmbedModel] = nil
				d.ReIndex = true
			} else {
				d.EmbedVectors[b.embedder.EmbedModel] = &StoredVectors{
					Embeddings: [][]float32{vecs},
					Regenerate: false,
				}
			}
		}
	}

	newDocumentSize := d.ApproximateMemorySize()
	d.IndexTime = FloatyNumber(time.Now().UTC().Unix())

	b.lock.Lock()
	defer b.lock.Unlock()
	if b.currentSizeBytes+newDocumentSize > b.MaxCommitSize {
		numCommitted, err = b.commitNoLock(true)
	} else if len(b.documents) >= b.MaxDocumentsCount {
		numCommitted, err = b.commitNoLock(false)
	}

	b.documents = append(b.documents, d)
	b.currentSizeBytes += newDocumentSize

	return
}

func (b *DocumentBatcher) Commit() (numCommitted int, err error) {
	b.lock.Lock()
	defer b.lock.Unlock()
	return b.commitNoLock(true)
}

func (b *DocumentBatcher) commitNoLock(required bool) (numCommitted int, err error) {
	if len(b.documents) == 0 {
		return
	}

	if b.lastTaskID != nil {
		// Wait for the last task to finish if necessary
		task, err := b.Index.GetTask(*b.lastTaskID)
		if err != nil {
			b.logger.Printf("Failed to get task %d: %s", *b.lastTaskID, err)
			goto out
		}

		if required && task.Status != meilisearch.TaskStatusSucceeded {
			b.logger.Printf("Previous task %d is in status %q", *b.lastTaskID, task.Status)
		}

		if task.Status == meilisearch.TaskStatusEnqueued || task.Status == meilisearch.TaskStatusProcessing {
			if !required {
				b.logger.Printf("Not committing while task %d is still running", *b.lastTaskID)
				return numCommitted, nil
			}

			b.logger.Printf("Waiting for task %d to finish", *b.lastTaskID)
			task, err = b.Index.WaitForTask(*b.lastTaskID, 500*time.Millisecond)
			if err != nil {
				b.logger.Printf("Failed to wait for task %d: %s", *b.lastTaskID, err)
				goto out
			}

			if task.Status != meilisearch.TaskStatusSucceeded {
				b.logger.Printf("Task %d did not succeed, status: %s", *b.lastTaskID, task.Status)
				goto out
			}

			b.lastTaskID = nil
		}
	}
out:

	// Attempt to add all documents
	numCommitted += b.divideAndConquer(b.documents, required)

	// Clear the batch after processing
	b.documents = nil
	b.currentSizeBytes = 0

	return
}

// divideAndConquer attempts to add documents recursively by splitting the batch
func (b *DocumentBatcher) divideAndConquer(docs []Document, required bool) int {
	if len(docs) == 0 {
		return 0
	}

	if required {
		b.logger.Printf("Trying to commit %d documents (%s)", len(docs), humanize.Bytes(uint64(sumDocumentBytes(docs))))
	}

	// Attempt to add the current batch of documents
	task, err := b.Index.AddDocuments(docs, nil)
	if err == nil {
		// Wait for the task to succeed
		taskInfo, err := b.Index.WaitForTask(task.TaskUID, 500*time.Millisecond)
		if err != nil {
			b.logger.Printf("Failed to wait for task %d: %s", task.TaskUID, err)
		} else if taskInfo.Status == meilisearch.TaskStatusSucceeded {
			b.logger.Printf("Successfully added %d documents", len(docs))
			return len(docs)
		} else {
			b.logger.Printf("Task %d did not succeed (%s), %s", task.TaskUID, taskInfo.Status, taskInfo.Error)
		}
	} else {
		if len(docs) == 1 {
			b.logger.Printf("Failed to add document %s: %s", docs[0].Slug, err)
			return 0
		} else {
			b.logger.Printf("Failed to add documents: %s", err)
		}
	}

	// Split the batch into two halves
	mid := len(docs) / 2
	left := docs[:mid]
	right := docs[mid:]

	// Recursively process both halves
	numCommitted := b.divideAndConquer(left, required)
	numCommitted += b.divideAndConquer(right, required)

	return numCommitted
}

func sumDocumentBytes(docs []Document) int {
	total := 0
	for _, doc := range docs {
		total += doc.ApproximateMemorySize()
	}
	return total
}
