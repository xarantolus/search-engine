package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"shared/doc"
	"strconv"
	"time"
)

type DocumentResponse struct {
	Limit   int64          `json:"limit"`
	Offset  int64          `json:"offset"`
	Total   int64          `json:"total"`
	Results []doc.Document `json:"results"`
}

var c = http.Client{
	Timeout: 2 * time.Minute,
}

func RequestDocuments(baseURL string, offset, limit int64, token string) (d DocumentResponse, err error) {
	url, err := url.Parse(baseURL)
	if err != nil {
		log.Fatalf("Failed to parse base URL: %v", err)
	}
	url.Path = "/api/v1/docs"

	query := url.Query()
	query.Set("offset", strconv.FormatInt(offset, 10))
	query.Set("limit", strconv.FormatInt(limit, 10))
	query.Set("apiKey", token)
	url.RawQuery = query.Encode()

	req, err := http.NewRequest("GET", url.String(), nil)
	if err != nil {
		return d, fmt.Errorf("failed to create request: %w", err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return d, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Read the response body to provide more context in the error
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return d, fmt.Errorf("unexpected status code: %d, and failed to read response body: %w", resp.StatusCode, readErr)
		}
		return d, fmt.Errorf("unexpected status code: %d, response body: %s", resp.StatusCode, body)
	}

	if err = json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return d, fmt.Errorf("failed to decode response: %w", err)
	}

	if d.Limit != limit {
		return d, fmt.Errorf("expected limit %d, got %d", limit, d.Limit)
	}
	if d.Offset != offset {
		return d, fmt.Errorf("expected offset %d, got %d", offset, d.Offset)
	}

	return d, nil
}
