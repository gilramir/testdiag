package tools

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gilramir/testdiag/internal/workspace"
)

const maxFunctionMatches = 100

// funcLangDef describes how to recognise source files and function-definition
// lines for one programming language.
type funcLangDef struct {
	// exts is the set of lower-case file extensions (without dot) for this language.
	exts map[string]bool
	// checkScript, when non-nil, is called for files with no extension that are
	// executable. It should return true if the file is a script in this language
	// (e.g. a Python shebang script).
	checkScript func(abs string, mode os.FileMode) bool
	// pattern builds the regexp that identifies a function-definition line.
	pattern func(funcName string) *regexp.Regexp
	// note is an optional caveat included in the tool result for this language.
	note string
}

var funcLangDefs = map[string]*funcLangDef{
	"python": {
		exts: map[string]bool{"py": true},
		// Also match executable scripts with a #!/...python shebang.
		checkScript: func(abs string, mode os.FileMode) bool {
			if mode&0111 == 0 {
				return false
			}
			f, err := os.Open(abs)
			if err != nil {
				return false
			}
			defer f.Close()
			sc := bufio.NewScanner(f)
			if sc.Scan() {
				line := sc.Text()
				return strings.HasPrefix(line, "#!") && strings.Contains(line, "python")
			}
			return false
		},
		pattern: func(name string) *regexp.Regexp {
			return regexp.MustCompile(`^\s*(?:async\s+)?def\s+` + regexp.QuoteMeta(name) + `\s*\(`)
		},
	},
	"Go": {
		exts: map[string]bool{"go": true},
		pattern: func(name string) *regexp.Regexp {
			// Matches plain functions and methods (with receiver).
			// [(\[ handles generics: func Foo[T any](...
			return regexp.MustCompile(`^func\s+(?:\([^)]*\)\s+)?` + regexp.QuoteMeta(name) + `\s*[(\[]`)
		},
	},
	"rust": {
		exts: map[string]bool{"rs": true},
		pattern: func(name string) *regexp.Regexp {
			return regexp.MustCompile(
				`^\s*(?:(?:pub(?:\s*\([^)]*\))?\s+)|(?:async\s+)|(?:unsafe\s+)|(?:extern\s+"[^"]*"\s+))*` +
					`fn\s+` + regexp.QuoteMeta(name) + `\s*[(\[]`)
		},
	},
	"C++": {
		exts: map[string]bool{
			"cc": true, "cpp": true, "cxx": true,
			"hh": true, "h": true, "hpp": true,
		},
		// C++ has no unambiguous single-line definition marker; return every line
		// where the name appears before '('. Declarations and calls may be included.
		pattern: func(name string) *regexp.Regexp {
			return regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\s*\(`)
		},
		note: "C++ results include definitions, declarations, and possibly calls — examine surrounding lines to identify the definition.",
	},
}

// ---------------------------------------------------------------------------
// function_lookup tool
// ---------------------------------------------------------------------------

type functionLookupTool struct{ ws *workspace.Workspace }

func (t *functionLookupTool) Name() string { return "function_lookup" }
func (t *functionLookupTool) Description() string {
	return "Find where a named function is defined in workspace source files. Language-aware: scans only files of the target language and matches definition syntax (def/func/fn keywords; for C++ finds all occurrences of name before '('). Searches recursively under the given directories. Returns workspace-relative file paths and 1-based line numbers."
}
func (t *functionLookupTool) JSONSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"language": map[string]interface{}{
				"type":        "string",
				"enum":        []string{"C++", "python", "Go", "rust"},
				"description": "Programming language of the function to find.",
			},
			"function_name": map[string]interface{}{
				"type":        "string",
				"description": "Exact name of the function (no parentheses or parameters).",
			},
			"directories": map[string]interface{}{
				"type":        "array",
				"items":       map[string]interface{}{"type": "string"},
				"description": `One or more workspace-relative directories to search recursively. Must be a JSON array of strings, e.g. ["src/b2"] or ["src/b2", "lib"].`,
			},
		},
		"required": []string{"language", "function_name", "directories"},
	}
}

func (t *functionLookupTool) Execute(ctx context.Context, args map[string]interface{}) (*Result, error) {
	lang, hasLang := strArg(args, "language")
	if !hasLang {
		return fail("function_lookup: 'language' is required")
	}
	funcName, hasFuncName := strArg(args, "function_name")
	if !hasFuncName {
		return fail("function_lookup: 'function_name' is required")
	}
	dirs, err := stringSlice(args, "directories")
	if err != nil || len(dirs) == 0 {
		return fail("function_lookup: 'directories' must be a non-empty array of workspace-relative paths")
	}

	def, found := funcLangDefs[lang]
	if !found {
		return fail("function_lookup: unsupported language %q; must be one of C++, python, Go, rust", lang)
	}
	re := def.pattern(funcName)

	// Resolve each directory; track which ones don't exist.
	type resolvedDir struct{ abs, rel string }
	var absDirs []resolvedDir
	var missingDirs []string
	for _, d := range dirs {
		abs, err := t.ws.Resolve(d)
		if err != nil {
			missingDirs = append(missingDirs, d)
			continue
		}
		info, statErr := os.Stat(abs)
		if statErr != nil || !info.IsDir() {
			missingDirs = append(missingDirs, d)
			continue
		}
		absDirs = append(absDirs, resolvedDir{abs: abs, rel: d})
	}

	type matchEntry struct {
		File string `json:"file"`
		Line int    `json:"line"`
	}

	var (
		matches      []matchEntry
		filesScanned int
		truncated    bool
	)

	for _, dir := range absDirs {
		if truncated {
			break
		}
		_ = filepath.WalkDir(dir.abs, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if d.IsDir() {
				if skipDirs[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}

			if !isLangFile(def, path, d) {
				return nil
			}
			filesScanned++

			lineNums, scanErr := scanForFunction(path, re)
			if scanErr != nil {
				return nil
			}
			for _, ln := range lineNums {
				if len(matches) >= maxFunctionMatches {
					truncated = true
					return filepath.SkipAll
				}
				matches = append(matches, matchEntry{
					File: t.ws.Rel(path),
					Line: ln,
				})
			}
			return nil
		})
	}

	result := map[string]interface{}{
		"language":      lang,
		"function_name": funcName,
		"files_scanned": filesScanned,
	}
	if len(missingDirs) > 0 {
		result["missing_directories"] = missingDirs
	}
	if truncated {
		result["truncated"] = true
	}
	if def.note != "" {
		result["note"] = def.note
	}

	switch {
	case len(absDirs) == 0:
		result["message"] = fmt.Sprintf("none of the specified directories exist: %s", strings.Join(missingDirs, ", "))
		result["matches"] = []matchEntry{}
	case filesScanned == 0:
		result["message"] = fmt.Sprintf("no %s source files found in the specified director%s",
			lang, pluralSuffix(len(absDirs), "y", "ies"))
		result["matches"] = []matchEntry{}
	case len(matches) == 0:
		result["message"] = fmt.Sprintf("function %q not found in %d %s file%s scanned",
			funcName, filesScanned, lang, pluralSuffix(filesScanned, "", "s"))
		result["matches"] = []matchEntry{}
	default:
		result["matches"] = matches
		result["count"] = len(matches)
	}
	return ok(result), nil
}

// isLangFile reports whether the directory entry is a source file for def.
func isLangFile(def *funcLangDef, path string, d os.DirEntry) bool {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	if ext != "" {
		return def.exts[ext]
	}
	// No extension: only checked when the language supports script detection.
	if def.checkScript == nil {
		return false
	}
	info, err := d.Info()
	if err != nil {
		return false
	}
	return def.checkScript(path, info.Mode())
}

// scanForFunction opens the file and returns 1-based line numbers of every line
// that matches re.
func scanForFunction(abs string, re *regexp.Regexp) ([]int, error) {
	f, err := os.Open(abs)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var lines []int
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	n := 0
	for sc.Scan() {
		n++
		if re.MatchString(sc.Text()) {
			lines = append(lines, n)
		}
	}
	return lines, sc.Err()
}

func pluralSuffix(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}
