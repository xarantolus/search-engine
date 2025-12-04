package extractors

import (
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/hhatto/gocloc"
)

var languages = gocloc.NewDefinedLanguages()

// Remove languages that aren't programming languages, or where I expect
// that tika will do a better job of extracting the relevant info.
func init() {
	var toRemove = []string{
		"Ascii",
		"HTML",
		"JSON",
		"Text",
		"XML",
		"Markdown",
		"YAML",
		"INI",
		"Properties",
	}

	for lang := range languages.Langs {
		loweredLang := strings.ToLower(lang)
		for _, removeStr := range toRemove {
			if strings.Contains(loweredLang, strings.ToLower(removeStr)) {
				delete(languages.Langs, lang)
				break
			}
		}
	}
}

// IsCodeFile returns true if the file is likely to contain code.
func IsCodeFile(fp string) bool {
	ext := filepath.Ext(fp)
	if len(ext) <= 1 {
		return false
	}

	languageForExt, ok := gocloc.Exts[strings.ToLower(strings.TrimLeft(ext, "."))]
	if !ok {
		return false
	}

	_, ok = languages.Langs[languageForExt]
	return ok
}

// ExtractCodeComments extracts comments from a file.
func ExtractCodeComments(fp string, reader io.Reader) (comments []string, err error) {
	var options = gocloc.NewClocOptions()
	options.OnComment = func(line string) {
		comments = append(comments, line)
	}
	ext := filepath.Ext(fp)
	if len(ext) <= 1 {
		return nil, fmt.Errorf("no file extension %s", ext)
	}

	languageForExt, ok := gocloc.Exts[strings.ToLower(strings.TrimLeft(ext, "."))]
	if !ok {
		return nil, fmt.Errorf("unknown file extension %s", ext)
	}

	definedLang, ok := languages.Langs[languageForExt]
	if !ok {
		return nil, fmt.Errorf("unknown language for extension %s", languageForExt)
	}

	_ = gocloc.AnalyzeReader(fp, definedLang, reader, options)

	return comments, nil
}

func ExtractCodeCommentsFile(fs fs.FS, fp string) (comments []string, err error) {
	f, err := fs.Open(fp)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	return ExtractCodeComments(fp, f)
}
