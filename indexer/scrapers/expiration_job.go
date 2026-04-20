package scrapers

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/meilisearch/meilisearch-go"
)

type ExpirationJob struct {
	OlderThan time.Duration

	Index meilisearch.IndexManager
}

func ExpirationMargin(olderThan time.Duration, factor float64) time.Duration {
	var margin = 1.5
	if olderThan >= 14*24*time.Hour {
		margin = 1.1
	} else if olderThan >= 7*24*time.Hour {
		margin = 1.25
	}

	return time.Duration(int64(float64(olderThan) * margin * factor))
}

// Deletes all documents last seen before the specified time, and add a bit of margin.
func (e *ExpirationJob) Run() error {
	logger := log.New(os.Stdout, fmt.Sprintf("[Expiration:%s] ", e.DisplayName()), log.LstdFlags)

	olderDuration := ExpirationMargin(e.OlderThan, 1)
	filter := fmt.Sprintf("indexTime < %d", time.Now().UTC().Add(-olderDuration).Unix())

	logger.Printf("Deleting documents older than %s", olderDuration)

	task, err := e.Index.DeleteDocumentsByFilter(filter, nil)
	if err != nil {
		return fmt.Errorf("failed to delete old documents: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	tsk, err := e.Index.WaitForTaskWithContext(ctx, task.TaskUID, 5*time.Second)
	if err != nil {
		return fmt.Errorf("failed to wait for deletion task: %w", err)
	}

	logger.Printf("Deleted %d documents", tsk.Details.DeletedDocuments)

	return nil
}

func (e *ExpirationJob) DisplayName() string {
	return fmt.Sprintf("Delete documents older than %s", e.OlderThan.String())
}

func (e *ExpirationJob) RunInterval() time.Duration {
	return time.Hour
}
