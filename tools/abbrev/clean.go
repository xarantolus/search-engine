package main

import (
	"fmt"
	"slices"
	"strings"
)

func dedupKey(key string) string {
	return strings.ToUpper(strings.ReplaceAll(key, " ", ""))
}

func normalizeRHS(rhs string) string {
	// Split into words, remove trailing 's' or 'n' from each word if lowercase
	words := strings.Fields(rhs)
	for i, w := range words {
		if len(w) > 1 {
			last := w[len(w)-1]
			if last == 's' || last == 'n' {
				// Only remove if lowercase
				if last == 's' && w[len(w)-1] == 's' {
					w = w[:len(w)-1]
				} else if last == 'n' && w[len(w)-1] == 'n' {
					w = w[:len(w)-1]
				}
			}
		}
		words[i] = w
	}
	replaced := strings.Join(words, "")
	replaced = strings.ReplaceAll(replaced, "-", "")
	replaced = strings.ReplaceAll(replaced, "_", "")
	replaced = strings.ReplaceAll(replaced, ".", "")

	return strings.ToLower(replaced)
}

// Remove entries that are strict prefixes of any other entry
func filterPrefixes(rhsList []string) []string {
	filtered := []string{}
	rhsSorted := make([]string, len(rhsList))
	copy(rhsSorted, rhsList)
	slices.SortFunc(rhsSorted, func(a, b string) int { return len(b) - len(a) })
	for i, item := range rhsSorted {
		isPrefix := false
		for j, other := range rhsSorted {
			if i != j && strings.HasPrefix(strings.ToLower(other), strings.ToLower(item)) {
				isPrefix = true
				break
			}
		}
		if !isPrefix {
			filtered = append(filtered, item)
		}
	}
	// Restore original order as much as possible
	out := []string{}
	for _, item := range rhsList {
		for _, f := range filtered {
			if item == f {
				out = append(out, item)
				break
			}
		}
	}
	return out
}

// Custom titlecase, similar to Python version
func customTitlecase(s string) string {
	// Fix split words like "Ex- Ploration" -> "Exploration"
	s = strings.ReplaceAll(s, "- ", "")
	shortWords := map[string]struct{}{
		// English
		"a": {}, "an": {}, "and": {}, "as": {}, "at": {}, "but": {}, "by": {}, "for": {}, "if": {}, "in": {}, "nor": {}, "of": {}, "on": {}, "or": {}, "so": {}, "the": {}, "to": {}, "up": {}, "is": {}, "it": {}, "be": {}, "this": {}, "that": {}, "with": {}, "from": {}, "into": {}, "over": {}, "about": {},
		// German
		"der": {}, "die": {}, "das": {}, "und": {}, "zu": {}, "von": {}, "mit": {}, "auf": {}, "für": {}, "bei": {}, "nach": {}, "über": {}, "durch": {}, "gegen": {}, "ohne": {}, "um": {}, "als": {},
	}
	words := strings.Fields(s)
	if len(words) == 0 {
		return ""
	}
	result := []string{}
	for i, word := range words {
		if word == strings.ToUpper(word) && len(word) > 1 {
			result = append(result, word)
		} else if i == 0 || i == len(words)-1 {
			result = append(result, strings.Title(strings.ToLower(word)))
		} else if _, ok := shortWords[strings.ToLower(word)]; ok {
			result = append(result, strings.ToLower(word))
		} else {
			result = append(result, strings.Title(strings.ToLower(word)))
		}
	}
	return strings.Join(result, " ")
}

// Clean and deduplicate abbreviations, returning a cleaned map
func cleanAbbreviations(filtered map[string][]string) map[string][]string {
	type cleanedEntry struct {
		Orig string
		RHS  []string
	}
	processed := map[string]*cleanedEntry{}

	// Collect all abbreviation variants for each deduplicated key
	variants := map[string][]string{}
	for abbr := range filtered {
		deduped := dedupKey(abbr)
		variants[deduped] = append(variants[deduped], abbr)
	}

	for deduped, abbrList := range variants {
		processed[deduped] = &cleanedEntry{Orig: strings.ToUpper(abbrList[0]), RHS: []string{}}

		// Add RHS values in frequency order - process abbreviations and their expansions
		seen := map[string]struct{}{}
		for _, abbr := range abbrList {
			for _, rhs := range filtered[abbr] {
				norm := normalizeRHS(rhs)
				if _, ok := seen[norm]; !ok {
					seen[norm] = struct{}{}
					processed[deduped].RHS = append(processed[deduped].RHS, rhs)
				}
			}
		}
	}

	// Deduplicate RHS values (case and space insensitive), filter prefixes
	for _, entry := range processed {
		seen := map[string]struct{}{}
		dedupedRHS := []string{}
		for _, rhs := range entry.RHS {
			norm := normalizeRHS(rhs)
			if _, ok := seen[norm]; !ok {
				seen[norm] = struct{}{}
				dedupedRHS = append(dedupedRHS, rhs)
			}
		}
		dedupedRHS = filterPrefixes(dedupedRHS)

		// Filter RHS entries to only include those starting with same letter as abbreviation
		matchingRHS := []string{}
		if len(entry.Orig) > 0 {
			abbrFirstLetter := strings.ToLower(string(entry.Orig[0]))
			for _, rhs := range dedupedRHS {
				if len(rhs) > 0 && strings.ToLower(string(rhs[0])) == abbrFirstLetter {
					matchingRHS = append(matchingRHS, rhs)
				}
			}
		}
		entry.RHS = matchingRHS
	}

	// Build cleaned map, only include entries with matching RHS
	cleaned := map[string][]string{}
	for _, entry := range processed {
		if len(entry.RHS) > 0 {
			cleaned[entry.Orig] = entry.RHS
		}
	}

	return cleaned
}

// Print cleaned abbreviations as Markdown
func printCleanedMarkdown(cleaned map[string][]string) {
	keys := make([]string, 0, len(cleaned))
	for k := range cleaned {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		formatted := []string{}
		for _, rhs := range cleaned[k] {
			formatted = append(formatted, customTitlecase(rhs))
		}
		fmt.Printf("- **%s**: %s\n", k, strings.Join(formatted, ", "))
	}
}
