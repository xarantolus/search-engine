package scrapers

import (
	"bytes"
	"context"
	"fmt"
	"hash/fnv"
	"indexer/scrapers/extractors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"shared/config"
	"sort"
	"strings"
	"unicode"

	"github.com/google/shlex"
	gitlab "gitlab.com/gitlab-org/api/client-go"
)

func hashURL(url string) string {
	h := fnv.New64a()
	h.Write([]byte(url))
	return fmt.Sprintf("%x", h.Sum(nil))
}

func runCommandString(hidden bool, command string, replaceVars bool) (stdout string, err error) {
	split, err := shlex.Split(command)
	if err != nil {
		return "", err
	}

	if replaceVars {
		type envVar struct {
			name  string
			value string
		}
		vars := []envVar{}
		for _, v := range os.Environ() {
			split := strings.SplitN(v, "=", 2)
			if len(split) != 2 {
				continue
			}
			vars = append(vars, envVar{name: split[0], value: split[1]})
		}
		sort.Slice(vars, func(i, j int) bool {
			return len(vars[i].name) > len(vars[j].name)
		})
		for i := range split {
			for _, v := range vars {
				split[i] = strings.ReplaceAll(split[i], "${"+v.name+"}", v.value)
				split[i] = strings.ReplaceAll(split[i], "$"+v.name, v.value)
			}
		}
	}

	return runCommand(hidden, split[0], split[1:]...)
}

func runCommand(hidden bool, command string, args ...string) (stdout string, err error) {
	return runCommandCtx(context.Background(), hidden, command, args...)
}

func runCommandCtx(ctx context.Context, hidden bool, command string, args ...string) (stdout string, err error) {
	cmd := exec.CommandContext(ctx, command, args...)
	var out bytes.Buffer
	if hidden {
		cmd.Stdout = &out
	} else {
		cmd.Stdout = io.MultiWriter(os.Stdout, &out)
		cmd.Stderr = os.Stderr
	}

	err = cmd.Run()
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(out.String()), nil
}

func Cascade(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func cleanFilename(str string) string {
	var nextRepeats = false
	return strings.Map(func(r rune) rune {
		// Basically, replace non-alphanumeric characters with _, but don't repeat it
		if r == '_' {
			if nextRepeats {
				return -1
			}
			nextRepeats = true
			return r
		}
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			nextRepeats = false
			return r
		}
		nextRepeats = true
		return '_'
	}, str)
}

func TextExtractionAllowed(includePattern string, excludePattern string, filename string) bool {
	if len(excludePattern) > 0 && AnyGlobMatches(excludePattern, filepath.Base(filename)) {
		return false
	}

	if len(includePattern) == 0 {
		return true
	}

	if extractors.IsCodeFile(filename) {
		return true
	}

	return AnyGlobMatches(includePattern, strings.ToLower(filepath.Base(filename)))
}

func AnyGlobMatches(includePattern string, filename string) bool {
	if len(includePattern) == 0 {
		panic("AnyGlobMatches called with empty includePattern")
	}
	includePattern = strings.ToLower(includePattern)
	filename = strings.ToLower(filename)

	split := strings.FieldsFunc(includePattern, func(r rune) bool {
		return r == ',' || r == ' '
	})

	for _, glob := range split {
		// we can't use (file)path.Match here, as both treat slashes as something special,
		// which we explicitly don't want.
		// E.g. path.Match("*/default", "group/subgroup/default") returns false, but I want true
		regexPattern := "^" + regexp.QuoteMeta(glob) + "$"
		regexPattern = strings.ReplaceAll(regexPattern, `\*`, ".*")
		regex, err := regexp.Compile(regexPattern)
		if err != nil {
			panic(fmt.Sprintf("Failed to compile glob %q to regex pattern %q: %v", glob, regexPattern, err))
		}

		if regex.MatchString(filename) {
			return true
		}
	}

	return false
}

// Use the first non-empty line that is not composed of just ----. Then if that line is a markdown title or has the title: something format, then use it as title. Otherwise use the base file name, but replace - with space
func markdownGetTitle(content, filename string) (title string) {
	lines := strings.Split(content, "\n")

	for _, line := range lines {
		trimmedLine := strings.TrimSpace(line)

		// Skip empty lines and lines composed of just ----
		if trimmedLine == "" || strings.Trim(trimmedLine, "-") == "" {
			continue
		}

		// Check for markdown title
		if strings.HasPrefix(trimmedLine, "#") {
			tl := strings.TrimLeft(trimmedLine, "#")
			numHashes := len(trimmedLine) - len(tl)

			// We only really consider titles with 1-3 hashes
			if numHashes > 0 && numHashes <= 3 && strings.HasPrefix(tl, " ") {
				return strings.TrimSpace(tl)
			}

			// could be a shebang or unrelated etc.
		}

		// Check for YAML title
		if strings.HasPrefix(trimmedLine, "title:") {
			yamlTitle := strings.TrimSpace(strings.TrimPrefix(trimmedLine, "title:"))
			if len(yamlTitle) >= 2 {
				firstChar := yamlTitle[0]
				lastChar := yamlTitle[len(yamlTitle)-1]
				if (firstChar == '\'' && lastChar == '\'') || (firstChar == '"' && lastChar == '"') {
					return yamlTitle[1 : len(yamlTitle)-1]
				}
			}
			return yamlTitle
		}

		// If we reach here, it means the line is not a title
		break
	}

	baseName := filepath.Base(filename)
	titleWithoutExt := strings.TrimSuffix(baseName, filepath.Ext(baseName))

	// Check if the title is not all uppercase and doesn't have spaces yet.
	if titleWithoutExt != strings.ToUpper(titleWithoutExt) && !strings.Contains(titleWithoutExt, " ") {
		titleWithoutExt = insertSpacesCamelCase(titleWithoutExt)
	}

	// Replace punctuation with space.
	var fileTitle = strings.Map(func(r rune) rune {
		if unicode.IsPunct(r) {
			return ' '
		}
		return r
	}, titleWithoutExt)

	if isOverlapping(title, fileTitle) {
		return strings.Join(strings.Fields(title), " ")
	}
	return strings.Join(strings.Fields(fileTitle), " ")
}

// Helper to insert spaces in a camel cased string.
func insertSpacesCamelCase(s string) string {
	runes := []rune(s)
	var result []rune
	for i, r := range runes {
		if i > 0 && unicode.IsUpper(r) {
			// Insert a space if previous character is lowercase.
			if unicode.IsLower(runes[i-1]) {
				result = append(result, ' ')
			}
			// Also, if next rune exists and is lowercase, insert space.
			if i+1 < len(runes) && unicode.IsLower(runes[i+1]) {
				result = append(result, ' ')
			}
		}
		result = append(result, r)
	}
	return string(result)
}

func isOverlapping(titleA, titleB string) bool {
	// Split words, clean words, and check if there is any overlap
	var splitWords = func(title string) []string {
		words := strings.Fields(title)
		for i, word := range words {
			// remove control characters and punctuation
			words[i] = strings.Map(func(r rune) rune {
				if unicode.IsControl(r) || unicode.IsPunct(r) {
					return -1
				}
				return r
			}, strings.ToLower(word))
		}
		return words
	}

	wordsA := splitWords(titleA)
	wordsB := splitWords(titleB)

	// Create a map to count occurrences of words in titleA
	wordCount := make(map[string]int)
	for _, word := range wordsA {
		wordCount[word]++
	}

	// Count overlapping words
	overlapCount := 0
	for _, word := range wordsB {
		if wordCount[word] > 0 {
			overlapCount++
		}
	}

	// Calculate the percentage of overlapping words
	totalWords := len(wordsA) + len(wordsB)
	if totalWords == 0 {
		return false
	}
	overlapPercentage := (2 * overlapCount * 100) / totalWords

	return overlapPercentage > 75
}

func GitLabClientFromURL(cfg *config.Config, gitlabURL string) (gc *gitlab.Client, err error) {
	urlParsed, err := url.Parse(gitlabURL)
	if err != nil {
		return
	}

	var token string
	for _, host := range cfg.GitLogins {
		if host.Host == urlParsed.Host {
			token = host.Token
			break
		}
	}
	if token == "" {
		return nil, fmt.Errorf("gitlab client: no token found for %s", urlParsed.Host)
	}

	baseUrl := *urlParsed
	baseUrl.Path = ""
	gc, err = gitlab.NewClient(token, gitlab.WithBaseURL(baseUrl.String()))
	if err != nil {
		return
	}

	return
}

func ListGitLabGroupProjects(client *gitlab.Client, groupID interface{}) (res []*gitlab.Project, err error) {
	options := &gitlab.ListGroupProjectsOptions{
		IncludeSubGroups: gitlab.Ptr(true),
		ListOptions: gitlab.ListOptions{
			Page:    1,
			PerPage: 100,
		},
	}

	for {
		projects, resp, err := client.Groups.ListGroupProjects(groupID, options)
		if err != nil {
			return res, err
		}

		res = append(res, projects...)

		if resp.CurrentPage >= resp.TotalPages {
			break
		}

		options.Page = resp.NextPage
	}

	return res, nil
}
