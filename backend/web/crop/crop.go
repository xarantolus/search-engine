package crop

import (
	"shared/config"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	ahocorasick "github.com/petar-dambovaliev/aho-corasick"
)

// ExtractRelevantTerms takes a query string and returns a slice of relevant terms.
// Quoted terms are treated as single entities, while unquoted terms are split by space.
// Terms that are inverted (prefixed with a minus sign) are excluded.
func ExtractRelevantTerms(query string, minLength int) []string {
	terms := []string{}
	currentTerm := ""
	inQuotes := false
	inverted := false

	// Convert query to runes to properly handle Unicode characters
	runes := []rune(query)

	for i := 0; i < len(runes); i++ {
		r := runes[i]

		if r == '"' {
			inQuotes = !inQuotes
			if inQuotes {
				// Check for - before quote (e.g., -"...")
				if i > 0 && runes[i-1] == '-' && (i == 1 || unicode.IsSpace(runes[i-2])) {
					inverted = true
					// Remove the '-' from currentTerm if present
					if len(currentTerm) > 0 && currentTerm[len(currentTerm)-1] == '-' {
						currentTerm = currentTerm[:len(currentTerm)-1]
					}
				}
			} else {
				// End of quoted term
				if currentTerm != "" {
					if !inverted && len(currentTerm) >= minLength {
						terms = append(terms, currentTerm)
					}
					currentTerm = ""
					inverted = false
				}
			}
			continue
		}

		if unicode.IsSpace(r) && !inQuotes {
			if currentTerm != "" {
				if !inverted && len(currentTerm) >= minLength {
					terms = append(terms, currentTerm)
				}
				currentTerm = ""
				inverted = false
			}
			continue
		}

		if r == '-' && !inQuotes && currentTerm == "" && (i == 0 || unicode.IsSpace(runes[i-1])) {
			inverted = true
			continue
		}

		currentTerm += string(r)
	}

	if currentTerm != "" {
		if !inverted && len(currentTerm) >= minLength {
			terms = append(terms, currentTerm)
		}
	}

	return terms
}

var matchBuilder = ahocorasick.NewAhoCorasickBuilder(ahocorasick.Opts{
	AsciiCaseInsensitive: true,
	MatchOnlyWholeWords:  false,
	MatchKind:            ahocorasick.LeftMostLongestMatch,
	DFA:                  true,
})

type ScoredParagraph struct {
	StartIndex   int
	EndIndex     int
	PatternCount []int
}

// Compares two paragraphs and returns the one with the higher score.
// Returns:
//   - negative value if p1 scores lower than p2
//   - zero if they score equally
//   - positive value if p1 scores higher than p2
func (p1 *ScoredParagraph) Compare(p2 *ScoredParagraph) int {
	// Calculate the number of unique terms in each paragraph
	uniqueTermsP1 := 0
	uniqueTermsP2 := 0

	// Calculate total term occurrences in each paragraph
	totalTermsP1 := 0
	totalTermsP2 := 0

	// Count unique terms and total occurrences
	for i, count := range p1.PatternCount {
		if count > 0 {
			uniqueTermsP1++
			totalTermsP1 += count
		}

		// Do the same for p2
		if p2.PatternCount[i] > 0 {
			uniqueTermsP2++
			totalTermsP2 += p2.PatternCount[i]
		}
	}

	// First comparison criterion: number of unique terms (diversity)
	if uniqueTermsP1 != uniqueTermsP2 {
		return uniqueTermsP1 - uniqueTermsP2
	}

	// Second comparison criterion: total occurrences (frequency)
	if totalTermsP1 != totalTermsP2 {
		return totalTermsP1 - totalTermsP2
	}

	// If all else is equal, prefer the shorter paragraph (for conciseness)
	p1Length := p1.EndIndex - p1.StartIndex
	p2Length := p2.EndIndex - p2.StartIndex

	// Return negative if p1 is longer (lower score), positive if p1 is shorter (higher score)
	return p1Length - p2Length
}

// ExpandTermsWithSynonyms expands the given terms by adding their synonyms from the provided SynonymsMap.
// Short terms (len < 3) are only added if they have synonyms; otherwise, they are skipped.
func ExpandTermsWithSynonyms(terms []string, synonyms config.SynonymsMap) []string {
	expandedTerms := make([]string, 0, len(terms))
	visited := make(map[string]bool)
	queue := make([]string, len(terms))

	// Initialize the queue with the original terms
	copy(queue, terms)

	for len(queue) > 0 {
		// Dequeue the first term
		term := strings.ToLower(queue[0])
		queue = queue[1:]

		// Skip if already visited
		if visited[term] {
			continue
		}
		visited[term] = true

		// Add the term to the expanded list
		expandedTerms = append(expandedTerms, term)

		// Get synonyms and enqueue them if not visited
		syn, ok := synonyms[term]
		if ok {
			for _, synonym := range syn {
				synonym = strings.ToLower(synonym)
				if !visited[synonym] {
					queue = append(queue, synonym)
				}
			}
		}
	}

	return expandedTerms
}

// CropRelevantContent takes a long content string and tries to efficiently crop
// it to sections that contain as many different relevant terms as possible.
// It dynamically determines the number of sections based on the difference in relevancy of
// the found paragraphs.
// Cropping is done by starting with a bit before the sentence that contains the first relevant term,
// and ending with a bit after the sentence that contains the last relevant term.
// Since we operate on text that is often not well formatted (due to text extraction from papers),
// we merge sentences together (if there's only one \n and there's no punctuation in between, it's likely a continuation of the same sentence).
// Also, we try to keep the paragraphs short, so that we don't end up with a huge chunk of text.
func CropRelevantContent(content string, query string, maxParagraphLength int, maxParagraphCount int, synonyms config.SynonymsMap) string {
	terms := ExtractRelevantTerms(query, 1)
	terms = append(terms, []string{"<mark>", "</mark>"}...)
	terms = ExpandTermsWithSynonyms(terms, synonyms)

	matcher := matchBuilder.Build(terms)

	// Find all matches in the content.
	matches := findMatches(matcher, content, 250)
	if len(matches) == 0 {
		// If no matches were found, return the first paragraph.
		endIndex := simpleFindParagraphEnding(content, maxParagraphLength)
		if endIndex == 0 {
			min := len(content)
			if min > maxParagraphLength {
				min = maxParagraphLength
			}
			return content[:min]
		}
		return content[:endIndex]
	}

	// Now for every match, find the start/end of the paragraph.
	// If an index is within a paragraph, it is used for scoring, but skipped afterwards.
	// We also have to ensure that they don't overlap.
	var paragraphs []ScoredParagraph
	for i := 0; i < len(matches); i++ {
		match := matches[i]

		// Find the start of the paragraph that contains the matchIndex.
		startIndex := simpleFindParagraphStart(content, match.Start(), maxParagraphLength/4)
		remainingLength := maxParagraphLength - (match.Start() - startIndex)
		endIndex := simpleFindParagraphEnding(content[match.Start():], remainingLength)
		endIndex += match.Start()

		for endIndex+1 < len(content) && !unicode.IsSpace(rune(content[endIndex+1])) &&
			!isSentenceEndingPunctuation(rune(content[endIndex+1])) {
			endIndex++
		}

		// Check if paragraph is too small and expand it if needed
		paragraphLength := endIndex - startIndex
		minDesiredLength := maxParagraphLength / 2

		if paragraphLength < minDesiredLength {
			// Calculate how much expansion we need
			expansionNeeded := minDesiredLength - paragraphLength

			// Try to expand start by finding a better paragraph boundary
			if startIndex > 0 && startIndex > 10 {
				// Look back further for a paragraph start, respecting sentence boundaries
				startIndex = simpleFindParagraphStart(content, startIndex-10, expansionNeeded/2)
			}

			// Try to expand end by finding a better paragraph boundary
			if endIndex < len(content) {
				// Get remaining content after current end
				remainingContent := content[endIndex:]
				// Find paragraph ending in the remaining content
				expandedEnd := simpleFindParagraphEnding(remainingContent, expansionNeeded/2)
				if expandedEnd > 0 {
					endIndex += expandedEnd
				}
			}

			// After trying sentence-aware expansion, ensure we meet minimum length
			if endIndex-startIndex < minDesiredLength {
				// Fallback: expand equally on both sides if still too small
				stillNeeded := minDesiredLength - (endIndex - startIndex)
				startExpansion := min(stillNeeded/2, startIndex)
				endExpansion := min(stillNeeded-startExpansion, len(content)-endIndex)

				startIndex = max(0, startIndex-startExpansion)
				endIndex = min(len(content), endIndex+endExpansion)
			}
		}

		// Check if this paragraph significantly overlaps with any existing paragraph
		overlaps := false
		for _, p := range paragraphs {
			// Skip if this paragraph is contained within or contains an existing paragraph
			if (startIndex >= p.StartIndex && endIndex <= p.EndIndex) ||
				(startIndex <= p.StartIndex && endIndex >= p.EndIndex) {
				overlaps = true
				break
			}
		}

		if overlaps {
			continue
		}

		// content[startIndex:endIndex] is our paragraph now. Score it based on the number of terms it contains,
		// and the diversity of the terms (a paragraph that contains a term 10 times)

		var patternCount []int = make([]int, len(terms))
		var j int
		for j = i + 1; j < len(matches); j++ {
			nextMatch := matches[j]
			if nextMatch.Start() >= endIndex {
				// If the next match is after the end of the current paragraph, we can stop.
				break
			}

			// This match is in the same paragraph
			patternCount[nextMatch.Pattern()]++
		}
		// We can skip the matches that are in the same paragraph, since we already counted them.
		i = j - 1

		// Add the current match to the pattern count
		patternCount[match.Pattern()]++
		paragraphs = append(paragraphs, ScoredParagraph{
			StartIndex:   startIndex,
			EndIndex:     endIndex,
			PatternCount: patternCount,
		})
	}

	// Now we have all paragraphs, we can sort them by score.
	if len(paragraphs) == 0 {
		// If no paragraphs were found, return the first paragraph.
		endIndex := simpleFindParagraphEnding(content, maxParagraphLength)
		if endIndex == 0 {
			min := len(content)
			if min > maxParagraphLength {
				min = maxParagraphLength
			}
			return content[:min]
		}
		return content[:endIndex]
	}

	// Sort paragraphs by score, top scoring first.
	sort.Slice(paragraphs, func(i, j int) bool {
		return paragraphs[i].Compare(&paragraphs[j]) > 0
	})

	// Now we have to dynamically find how many paragraphs we want to return (-> dynamic threshold)

	// Calculate paragraph scores for thresholding
	type ScoredIndex struct {
		Index int
		Score float64
	}

	// Calculate scores for each paragraph
	scores := make([]ScoredIndex, len(paragraphs))
	for i := 0; i < len(paragraphs); i++ {
		// Use a scoring function that considers both diversity and frequency
		uniqueTerms := 0
		totalOccurrences := 0

		for _, count := range paragraphs[i].PatternCount {
			if count > 0 {
				uniqueTerms++
				totalOccurrences += count
			}
		}

		// Normalized score calculation (BM25-inspired)
		var score float64
		if uniqueTerms > 0 {
			// Term coverage component (diversity)
			coverage := float64(uniqueTerms) / float64(len(terms))

			// Frequency component with saturation
			k1 := 1.2 // Saturation parameter
			avgFreq := float64(totalOccurrences) / float64(uniqueTerms)
			saturatedFreq := avgFreq * (k1 + 1) / (avgFreq + k1)

			// Combined score (higher weight on coverage)
			score = (coverage * 0.7) + (saturatedFreq * 0.3)
		}

		scores[i] = ScoredIndex{Index: i, Score: score}
	}

	// Find the elbow point in the scores
	selectedIndices := make(map[int]bool)

	// Always include the top-scoring paragraph
	if len(paragraphs) > 0 {
		selectedIndices[0] = true
	}

	// We're looking for the point where adding more paragraphs gives diminishing returns
	if len(paragraphs) > 1 {
		// Minimum score threshold (relative to top score)
		topScore := scores[0].Score
		minScoreThreshold := topScore * 0.5 // At least 50% as relevant as the top paragraph

		// Maximum rate of score drop we consider acceptable
		maxDropRate := 0.4 // 40% drop between consecutive paragraphs

		// Select paragraphs until we hit the elbow point
		for i := 1; i < len(scores) && len(selectedIndices) < maxParagraphCount; i++ {
			currentScore := scores[i].Score
			previousScore := scores[i-1].Score

			// Stop if score drops below minimum threshold
			if currentScore < minScoreThreshold {
				break
			}

			// Calculate percentage drop from previous score
			dropRate := 0.0
			if previousScore > 0 {
				dropRate = (previousScore - currentScore) / previousScore
			}

			// Stop if drop rate exceeds our maximum
			if dropRate > maxDropRate {
				break
			}

			// This paragraph passed our tests, so include it
			selectedIndices[scores[i].Index] = true
		}
	}

	// Convert map to slice of selected paragraphs
	selectedParagraphs := make([]ScoredParagraph, 0, len(selectedIndices))
	for idx := range selectedIndices {
		selectedParagraphs = append(selectedParagraphs, paragraphs[idx])
	}

	// Now we have the elbow point, we can sort and return in original order
	sort.Slice(selectedParagraphs, func(i, j int) bool {
		return selectedParagraphs[i].StartIndex < selectedParagraphs[j].StartIndex
	})

	// Combine the selected paragraphs
	if len(selectedParagraphs) == 0 {
		// Handle edge case (shouldn't happen with proper implementation)
		return content[:min(len(content), maxParagraphLength)]
	}
	if len(selectedParagraphs) == 1 {
		// Just return the single selected paragraph
		return content[selectedParagraphs[0].StartIndex:selectedParagraphs[0].EndIndex]
	}
	// Combine multiple paragraphs, potentially with indicators between them
	var result string
	for i, para := range selectedParagraphs {
		if i > 0 {
			result += "\n[...]\n" // Indicate omitted content
		}
		result += content[para.StartIndex:para.EndIndex]
	}
	return result
}

// like ahocorasick.FindAll, but with a limit
func findMatches(ac ahocorasick.AhoCorasick, haystack string, limit int) []ahocorasick.Match {
	iter := ac.Iter(haystack)
	matches := make([]ahocorasick.Match, 0)

	for len(matches) < limit {
		next := iter.Next()
		if next == nil {
			break
		}

		matches = append(matches, *next)
	}

	return matches
}

func simpleFindParagraphStart(content string, matchIndex int, maxLength int) int {
	// We want to find the start of the paragraph that contains the matchIndex.
	// We will go backwards until we find a newline or reach the beginning of the content.
	var lastWasNewline bool
	var startIndex int

	// iterate string backwards from the matchIndex, but as runes

	var i int
	for i = matchIndex; i > 0; {
		// Find the beginning of the last rune
		start := i - 1
		for start > 0 && (content[start]&0xC0) == 0x80 {
			start--
		}
		r, size := utf8.DecodeRuneInString(content[start:i])
		i = start

		// Find the start of the paragraph by looking for multiple newlines,
		// the beginning of the string. or a sentence-ending punctuation (only if maxlength exceeded).
		if r == '\r' {
			continue
		}
		if r == '\n' {
			if lastWasNewline {
				// If we have two newlines in a row, we consider this the start of a paragraph.
				startIndex = i + 2*size
				break
			}
			lastWasNewline = true
			continue
		}
		lastWasNewline = false

		if isSentenceEndingPunctuation(r) {
			startIndex = i + size
			break
		}
	}
	if i == 0 {
		startIndex = 0
	}

	return startIndex
}

// findParagraphEnding finds the end of a paragraph starting from the given index,
// ensuring that we don't cut off a in the middle of a sentence.
func simpleFindParagraphEnding(content string, maxLength int) int {
	var lastWasNewline bool
	endIndex := 0
	minLength := maxLength / 2

	for i, r := range content {
		if i >= maxLength {
			endIndex = i
			break
		}
		if r == '\r' {
			continue
		}
		if r == '\n' {
			if lastWasNewline && i >= minLength {
				// Only consider paragraph end if we've reached at least half the max length
				endIndex = i - 1
				break
			}
			lastWasNewline = true
			continue
		}
		lastWasNewline = false

		// Check for punctuation that typically ends a sentence.
		if isSentenceEndingPunctuation(r) && i >= minLength {
			if i+1 < len(content) {
				nextRune, _ := utf8.DecodeRuneInString(content[i+1:])
				if unicode.IsSpace(nextRune) {
					endIndex = i + 1 // Include the punctuation in the end index
					break
				}
			} else {
				// If at the end of content, treat as end of sentence
				endIndex = i + 1
				break
			}
		}
	}

	// If we never found a good end, return the furthest we got
	if endIndex == 0 {
		if len(content) < maxLength {
			endIndex = len(content)
		} else {
			endIndex = maxLength
		}
	}

	return endIndex
}

// Helper function to determine minimum of two integers
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func isSentenceEndingPunctuation(r rune) bool {
	// These are the common sentence-ending punctuation marks
	return r == '.' || r == '!' || r == '?'
}
