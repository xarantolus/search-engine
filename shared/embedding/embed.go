package embedding

import (
	"context"
	"fmt"
	"time"

	"github.com/openai/openai-go"
)

type EmbedConfig struct {
	EmbedModel string
	Embedder   *openai.Client
	MaxTokens  int
	Dimensions int
}

func (e *EmbedConfig) EmbedText(text string, isQuery bool, userContext ...context.Context) (embedding []float32, err error) {
	var ctx context.Context
	if len(userContext) > 0 {
		ctx = userContext[0]
	} else {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	// We try to embed the document until it works. Of course we could count tokens etc.,
	// but this is annoying when we want to change the model.
	var cropLengths = []int{len(text), e.MaxTokens * 16, e.MaxTokens * 8, e.MaxTokens * 3, e.MaxTokens * 2, e.MaxTokens}

	var userName string
	if isQuery {
		userName = "query"
	}

	var embeddings *openai.CreateEmbeddingResponse
	for _, l := range cropLengths {
		if l > len(text) {
			continue
		}

		embeddings, err = e.Embedder.Embeddings.New(ctx, openai.EmbeddingNewParams{
			Input: openai.EmbeddingNewParamsInputUnion{
				OfArrayOfStrings: []string{
					text[:l],
				},
			},
			EncodingFormat: openai.EmbeddingNewParamsEncodingFormatFloat,
			Model:          openai.EmbeddingModel(e.EmbedModel),
			User:           openai.Opt(userName),
		})
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, err
	}

	// We only need the first embedding
	if len(embeddings.Data) == 0 {
		return nil, fmt.Errorf("no embeddings returned")
	}
	if e.Dimensions > 0 && len(embeddings.Data[0].Embedding) != e.Dimensions {
		return nil, fmt.Errorf("embedding has wrong dimensions: %d != %d", len(embeddings.Data[0].Embedding), e.Dimensions)
	}

	// Convert []float64 to []float32
	embedding = make([]float32, len(embeddings.Data[0].Embedding))
	for i, v := range embeddings.Data[0].Embedding {
		embedding[i] = float32(v)
	}

	return embedding, err
}
