package abbrev

import (
	"regexp"
	"strings"
	"unicode"
)

var bracesRegex = regexp.MustCompile(`\(([A-Za-z][A-Za-z0-9]{1,})\)`)

// ExtractAbbreviations extracts abbreviations and their corresponding full text mappings from the input text.
func ExtractAbbreviations(text string, maxCount int) map[string][]string {
	mapping := make(map[string][]string)
	matches := bracesRegex.FindAllStringSubmatchIndex(text, maxCount)

	for _, match := range matches {
		if len(match) < 4 {
			continue
		}
		abbr := text[match[2]:match[3]]
		abbrLower := strings.ToLower(abbr)
		abbrLen := len(abbr)
		abbrIdx := match[0]

		// Exclude abbreviations that are all digits or single letters
		if abbrLen < 2 || isAllDigits(abbr) {
			continue
		}

		// Exclude abbreviations with any character that is not a unicode letter, digit, or dash (Pd category)
		invalid := false
		for _, r := range abbr {
			if !(unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.In(r, unicode.Dash)) {
				invalid = true
				break
			}
		}
		if invalid {
			continue
		}

		// Exclude abbreviations that are embedded within a word (both sides are word characters)
		leftWord := abbrIdx > 0 && isWordChar(rune(text[abbrIdx-1]))
		abbrEnd := match[1]
		rightWord := abbrEnd < len(text) && isWordChar(rune(text[abbrEnd]))
		if leftWord && rightWord {
			continue
		}

		// Exclude abbreviations that are immediately preceded by '=' or '.'
		parenIdx := abbrIdx - 1
		if parenIdx > 0 && (text[parenIdx-1] == '=' || text[parenIdx-1] == '.') {
			continue
		}

		// Scan backwards for a few words before the abbreviation, keeping offsets
		const backwardsWordLimit = 15
		start := abbrIdx - 1
		wordsWithOffsets := splitWordsWithOffsets(text, 0, start+1)
		if len(wordsWithOffsets) > backwardsWordLimit {
			wordsWithOffsets = wordsWithOffsets[len(wordsWithOffsets)-backwardsWordLimit:]
		}
		if len(wordsWithOffsets) == 0 {
			continue
		}
		wordsWithOffsets = mergeHyphenatedWordsWithNewline(wordsWithOffsets, text)

		// Try all possible windows from the end
		var bestMatch []string
		for winStart := 0; winStart < len(wordsWithOffsets); winStart++ {
			candidateOffsets := wordsWithOffsets[winStart:]
			candidate := make([]string, len(candidateOffsets))
			for i, w := range candidateOffsets {
				candidate[i] = w.Word
			}

			// Try unified matching that can handle both simple and complex cases
			matchedWordIdxs := unifiedMatch(abbr, candidate)

			// If no match found, try single-word acronym matching
			if matchedWordIdxs == nil {
				for i, word := range candidate {
					if matchesSingleWordAcronym(abbr, word) {
						matchedWordIdxs = []int{i}
						break
					}
				}
			}

			if len(matchedWordIdxs) >= 1 {
				startIdx := matchedWordIdxs[0]

				// Include any words between the last matched word and the abbreviation position
				// This handles cases where there might be additional descriptive text
				endIdx := len(candidate) - 1

				// Only allow if the span is not too large
				if endIdx-startIdx+1 > abbrLen+5 {
					continue
				}
				resultWords := candidate[startIdx : endIdx+1]

				// Prefer shorter matches (closer to the abbreviation)
				if bestMatch == nil || len(resultWords) < len(bestMatch) {
					bestMatch = resultWords
				}
			}
		}

		if bestMatch != nil {
			// No need to merge hyphenated line breaks here since it's already done by mergeHyphenatedWordsWithNewline
			mergedWords := bestMatch

			fullText := strings.Join(mergedWords, " ")
			fullText = strings.ReplaceAll(fullText, "\n", " ")
			fullText = strings.ReplaceAll(fullText, "\r", " ")
			fullText = strings.Join(strings.Fields(fullText), " ")
			fullText = strings.Trim(fullText, " \t\n\r\"'.,:;!?")
			fullText = strings.TrimFunc(fullText, func(r rune) bool {
				return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.In(r, unicode.Dash)
			})

			if strings.EqualFold(fullText, abbr) || strings.Contains(fullText, "="+abbr) {
				continue
			}
			if len(fullText) > 0 {
				allowed := func(s string) bool {
					for _, r := range s {
						if !(unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.In(r, unicode.Dash) || r == ' ' || r == '\'' || r == '"') {
							return false
						}
					}
					return true
				}

				wordCount := len(strings.Fields(fullText))
				if allowed(abbr) && allowed(fullText) && wordCount >= 1 {
					alreadyExists := false
					for _, v := range mapping[abbrLower] {
						if v == fullText {
							alreadyExists = true
							break
						}
					}
					if !alreadyExists {
						mapping[abbrLower] = append(mapping[abbrLower], fullText)
					}
				}
			}
		}
	}
	return mapping
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

type wordWithOffset struct {
	Word   string
	Offset int // offset in original text
}

// Split text into words, keeping the offset of each word in the original text
func splitWordsWithOffsets(text string, start, end int) []wordWithOffset {
	var words []wordWithOffset
	inWord := false
	wordStart := 0
	for i := start; i < end; i++ {
		if unicode.IsSpace(rune(text[i])) {
			if inWord {
				words = append(words, wordWithOffset{
					Word:   text[wordStart:i],
					Offset: wordStart,
				})
				inWord = false
			}
		} else {
			if !inWord {
				wordStart = i
				inWord = true
			}
			if i == end-1 {
				words = append(words, wordWithOffset{
					Word:   text[wordStart : i+1],
					Offset: wordStart,
				})
			}
		}
	}
	return words
}

// Merge hyphenated words only if the next word started with a newline in the original text
func mergeHyphenatedWordsWithNewline(words []wordWithOffset, originalText string) []wordWithOffset {
	var merged []wordWithOffset
	i := 0
	for i < len(words) {
		word := words[i].Word
		if strings.HasSuffix(word, "-") && i+1 < len(words) {
			next := words[i+1]
			afterWord := originalText[words[i].Offset+len(word) : next.Offset]
			if strings.Contains(afterWord, "\n") {
				// Merge, offset is the first word's offset
				merged = append(merged, wordWithOffset{
					Word:   word[:len(word)-1] + next.Word,
					Offset: words[i].Offset,
				})
				i += 2
				continue
			}
		}
		merged = append(merged, words[i])
		i++
	}
	return merged
}

func isWordChar(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_'
}

// matchesSingleWordAcronym checks if an abbreviation matches a single word as an acronym
// For example: "OPS" matches "Operations", "COM" matches "Communications"
func matchesSingleWordAcronym(abbr, word string) bool {
	abbrRunes := []rune(strings.ToLower(abbr))
	wordRunes := []rune(strings.ToLower(word))

	if len(abbrRunes) == 0 || len(wordRunes) == 0 {
		return false
	}

	// Simple case: first letters match and word is long enough
	if len(abbrRunes) <= len(wordRunes) && abbrRunes[0] == wordRunes[0] {
		// For single letter abbreviations, just check first letter
		if len(abbrRunes) == 1 {
			return true
		}

		// For multi-letter abbreviations, try to match subsequent letters
		// to consonants or prominent characters in the word
		abbrIdx := 1
		wordIdx := 1

		for abbrIdx < len(abbrRunes) && wordIdx < len(wordRunes) {
			abbrChar := abbrRunes[abbrIdx]

			// Look for the abbreviation character in the remaining word
			found := false
			for wordIdx < len(wordRunes) {
				if wordRunes[wordIdx] == abbrChar {
					found = true
					wordIdx++
					break
				}
				wordIdx++
			}

			if !found {
				return false
			}

			abbrIdx++
		}

		// All abbreviation letters were found in order
		return abbrIdx == len(abbrRunes)
	}

	return false
}

// unifiedMatch performs comprehensive abbreviation matching
// It tries multiple strategies: first-letter matching, within-word matching, and allows skipping
func unifiedMatch(abbr string, candidate []string) []int {
	if len(abbr) == 0 || len(candidate) == 0 {
		return nil
	}

	abbrRunes := []rune(strings.ToLower(abbr))

	// Try to match using backtracking with all strategies including character skipping
	var result []int
	if matchRecursive(abbrRunes, candidate, 0, 0, []int{}, &result) {
		return result
	}

	return nil
}

// matchRecursive performs recursive matching with backtracking
func matchRecursive(abbr []rune, words []string, abbrIdx int, wordIdx int, currentMatch []int, result *[]int) bool {
	// Base case: all abbreviation letters matched
	if abbrIdx >= len(abbr) {
		*result = make([]int, len(currentMatch))
		copy(*result, currentMatch)
		return true
	}

	// Base case: no more words to check
	if wordIdx >= len(words) {
		return false
	}

	word := strings.ToLower(words[wordIdx])

	// Strategy 1: Try to match multiple letters from current word
	// Handle hyphenated words by splitting them
	wordParts := strings.Split(word, "-")
	lettersMatched := 0
	tempAbbrIdx := abbrIdx

	// Try to match letters from all parts of the (possibly hyphenated) word
	for _, part := range wordParts {
		for _, wordChar := range part {
			if tempAbbrIdx < len(abbr) && abbr[tempAbbrIdx] == wordChar {
				lettersMatched++
				tempAbbrIdx++
			}
		}
	}

	// If we matched at least one letter from this word, try continuing
	if lettersMatched > 0 {
		newMatch := append(currentMatch, wordIdx)
		if matchRecursive(abbr, words, tempAbbrIdx, wordIdx+1, newMatch, result) {
			return true
		}
	}

	// Strategy 2: Skip current word and try next (allow up to 2 consecutive skips)
	skipCount := 0
	for skip := wordIdx + 1; skip < len(words) && skipCount < 2; skip++ {
		skipCount++
		skipWord := strings.ToLower(words[skip])
		skipWordParts := strings.Split(skipWord, "-")

		// Check if this word can contribute to the match
		skipLettersMatched := 0
		tempAbbrIdx := abbrIdx
		for _, part := range skipWordParts {
			for _, wordChar := range part {
				if tempAbbrIdx < len(abbr) && abbr[tempAbbrIdx] == wordChar {
					skipLettersMatched++
					tempAbbrIdx++
				}
			}
		}

		if skipLettersMatched > 0 {
			newMatch := append(currentMatch, skip)
			if matchRecursive(abbr, words, tempAbbrIdx, skip+1, newMatch, result) {
				return true
			}
		}
	}

	// Strategy 3: Skip characters in abbreviation (only for digits or if no other match found)
	// This is more restrictive - only skip if current character is a digit or special character
	if abbrIdx < len(abbr) && (unicode.IsDigit(abbr[abbrIdx]) || (!unicode.IsLetter(abbr[abbrIdx]))) {
		if matchRecursive(abbr, words, abbrIdx+1, wordIdx, currentMatch, result) {
			return true
		}
	}

	return false
}
