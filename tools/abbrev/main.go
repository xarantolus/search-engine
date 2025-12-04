package main

import (
	"abbrev/abbrev"
	"flag"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"
)

func saveYAML(filename string, data any) error {
	f, err := os.Create(filename)
	if err != nil {
		return fmt.Errorf("failed to create output file %s: %w", filename, err)
	}
	defer f.Close()

	yamlEncoder := yaml.NewEncoder(f)
	yamlEncoder.SetIndent(2)
	if err := yamlEncoder.Encode(data); err != nil {
		return fmt.Errorf("failed to encode YAML: %w", err)
	}
	if err := yamlEncoder.Close(); err != nil {
		return fmt.Errorf("failed to close YAML encoder: %w", err)
	}
	return nil
}

type abbreviation struct {
	Count int
	Text  string
}

type docJob struct {
	Content string
}

type abbrResult struct {
	Abbrevs map[string][]string
}

func main() {
	var (
		flagBaseURL = flag.String("base-url", "http://localhost:8090", "Base URL of the search-engine instance")
		token       = flag.String("token", "", "API token for authentication - must set is_admin to true in config.")
		outputFile  = flag.String("output", "abbreviations.yaml", "Output file to save the abbreviations")
		inputLimit  = flag.Int64("limit", -1, "Number of documents to process (default: all)")
	)
	flag.Parse()
	if *token == "" {
		panic("You must provide a token with --token")
	}
	const limit int64 = 1000

	numWorkers := runtime.NumCPU() * 3
	jobs := make(chan docJob, limit*5)
	results := make(chan abbrResult, limit)
	var wg sync.WaitGroup

	// Worker pool
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				abbrevs := abbrev.ExtractAbbreviations(job.Content, 5000)
				if len(abbrevs) == 0 {
					continue
				}
				results <- abbrResult{Abbrevs: abbrevs}
			}
		}()
	}

	var abbreviations = make(map[string][]abbreviation)
	var offset int64 = 0
	var totalDocs int
	var mu sync.Mutex

	// Collector goroutine
	var collectorWg sync.WaitGroup
	collectorWg.Add(1)
	go func() {
		defer collectorWg.Done()
		for res := range results {
			mu.Lock()
			for abbr, texts := range res.Abbrevs {
				if len(abbr) <= 1 {
					continue
				}
				if _, exists := abbreviations[abbr]; !exists {
					abbreviations[abbr] = []abbreviation{}
				}
				for _, text := range texts {
					if !slices.ContainsFunc(abbreviations[abbr], func(a abbreviation) bool {
						return a.Text == text
					}) {
						abbreviations[abbr] = append(abbreviations[abbr], abbreviation{Count: 1, Text: text})
					} else {
						for i := range abbreviations[abbr] {
							if abbreviations[abbr][i].Text == text {
								abbreviations[abbr][i].Count++
								break
							}
						}
					}
				}
			}
			mu.Unlock()
		}
	}()

	for {
		start := time.Now()
		d, err := RequestDocuments(*flagBaseURL, offset, limit, *token)
		if err != nil {
			panic(err)
		}
		end := time.Now()
		if len(d.Results) == 0 {
			break
		}
		for _, doc := range d.Results {
			if doc.IsCode || len(doc.Content) < 10 {
				continue
			}
			jobs <- docJob{Content: doc.Content}
			totalDocs++
			if *inputLimit > 0 && int64(totalDocs) >= *inputLimit {
				fmt.Printf("Reached input limit of %d documents\n", *inputLimit)
				break
			}
		}
		mu.Lock()
		count := len(abbreviations)
		mu.Unlock()
		fmt.Printf("Queued %d documents so far (offset %d, request %s, fill %d, abbreviations %d)\n", totalDocs, offset, end.Sub(start).Round(time.Millisecond), len(jobs), count)
		offset += limit
	}
	close(jobs)
	wg.Wait()
	close(results)
	collectorWg.Wait()

	fmt.Printf("Processed %d documents\n", totalDocs)
	fmt.Printf("Found %d abbreviations in total\n", len(abbreviations))

	if err := saveYAML(*outputFile, abbreviations); err != nil {
		panic(err)
	}
	fmt.Printf("Abbreviations saved to %s\n", *outputFile)

	// Build and save the filtered map[string][]string (count > 5)
	filtered := make(map[string][]string)
	for abbr, arr := range abbreviations {
		// Sort by count descending
		slices.SortFunc(arr, func(a, b abbreviation) int {
			return b.Count - a.Count
		})
		for _, a := range arr {
			if a.Count > 5 {
				filtered[abbr] = append(filtered[abbr], a.Text)
			}
		}
	}
	fmt.Printf("Filtered abbreviations (count > 5): %d\n", len(filtered))
	filteredFile := *outputFile + ".filtered.yaml"
	if err := saveYAML(filteredFile, filtered); err != nil {
		panic(err)
	}
	fmt.Printf("Filtered abbreviations (count > 5) saved to %s\n", filteredFile)

	cleaned := cleanAbbreviations(filtered)
	cleanedFile := *outputFile + ".cleaned.yaml"
	if err := saveYAML(cleanedFile, cleaned); err != nil {
		panic(err)
	}
	fmt.Printf("Cleaned abbreviations saved to %s\n", cleanedFile)

	fmt.Println("\nCleaned abbreviations (Markdown):")
	printCleanedMarkdown(cleaned)
}

// Normalize a phrase for deduplication: lowercase, remove punctuation, collapse whitespace.
func normalizePhrase(s string) string {
	var b strings.Builder
	lastSpace := false
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
			lastSpace = false
		} else if unicode.IsSpace(r) {
			if !lastSpace {
				b.WriteRune(' ')
				lastSpace = true
			}
		}
		// else skip punctuation
	}
	return strings.TrimSpace(b.String())
}

// Simple Jaccard similarity on word sets
func jaccard(a, b string) float64 {
	wa := strings.Fields(a)
	wb := strings.Fields(b)
	setA := make(map[string]struct{}, len(wa))
	setB := make(map[string]struct{}, len(wb))
	for _, w := range wa {
		setA[w] = struct{}{}
	}
	for _, w := range wb {
		setB[w] = struct{}{}
	}
	inter, union := 0, len(setA)
	for w := range setB {
		if _, ok := setA[w]; ok {
			inter++
		} else {
			union++
		}
	}
	if union == 0 {
		return 1.0
	}
	return float64(inter) / float64(union)
}

// Deduplicate abbreviation expansions by similarity
func deduplicateAbbrMap(in map[string][]string) map[string][]string {
	const similarityThreshold = 0.8 // adjust as needed
	out := make(map[string][]string, len(in))
	for abbr, arr := range in {
		var kept []string
		var keptNorm []string
		for _, s := range arr {
			norm := normalizePhrase(s)
			tooSimilar := false
			for _, kn := range keptNorm {
				if jaccard(norm, kn) >= similarityThreshold {
					tooSimilar = true
					break
				}
			}
			if !tooSimilar {
				kept = append(kept, s)
				keptNorm = append(keptNorm, norm)
			}
		}
		if len(kept) > 0 {
			out[abbr] = kept
		}
	}
	return out
}
