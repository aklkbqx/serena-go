package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var reGitHunk = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

type lineSpan struct {
	start int
	end   int
}

func parseUnifiedDiffSpans(diffText string) map[string][]lineSpan {
	out := map[string][]lineSpan{}
	curFile := ""
	for _, ln := range strings.Split(diffText, "\n") {
		if strings.HasPrefix(ln, "+++ b/") {
			curFile = filepath.ToSlash(strings.TrimPrefix(ln, "+++ b/"))
			continue
		}
		if strings.HasPrefix(ln, "+++ /dev/null") {
			curFile = ""
			continue
		}
		if curFile == "" {
			continue
		}
		if m := reGitHunk.FindStringSubmatch(ln); m != nil {
			st, _ := strconv.Atoi(m[1])
			cnt := 1
			if m[2] != "" {
				cnt, _ = strconv.Atoi(m[2])
			}
			if cnt <= 0 {
				cnt = 1
			}
			out[curFile] = append(out[curFile], lineSpan{start: st, end: st + cnt - 1})
		}
	}
	return out
}

func spansOverlap(aStart, aEnd int, spans []lineSpan) bool {
	for _, s := range spans {
		if aStart <= s.end && aEnd >= s.start {
			return true
		}
	}
	return false
}

// toolChangedSymbols maps git diff/staged hunks to modified AST symbols + top callers + live diagnostics.
func toolChangedSymbols(repoRoot string, args map[string]any) (string, error) {
	base := strings.TrimSpace(argString(args, "base"))
	staged := argBool(args, "staged", false)
	incCallers := argBool(args, "include_callers", true)
	incDiags := argBool(args, "include_diagnostics", true)
	maxChars := argInt(args, "max_chars", 6000)
	if maxChars <= 0 {
		maxChars = 6000
	}

	gitArgs := []string{"-C", repoRoot, "diff", "--unified=0", "--no-color"}
	if staged {
		gitArgs = append(gitArgs, "--cached")
	} else if base != "" {
		gitArgs = append(gitArgs, base)
	} else {
		gitArgs = append(gitArgs, "HEAD")
	}
	out, err := exec.Command("git", gitArgs...).Output()
	if err != nil {
		// Fallback for repos without HEAD commit yet
		out, err = exec.Command("git", "-C", repoRoot, "diff", "--unified=0", "--no-color").Output()
		if err != nil {
			return "", fmt.Errorf("git diff failed: %w", err)
		}
	}
	fileSpans := parseUnifiedDiffSpans(string(out))
	if len(fileSpans) == 0 {
		return "No modified code hunks detected in git diff.", nil
	}

	files := make([]string, 0, len(fileSpans))
	for f := range fileSpans {
		files = append(files, f)
	}
	sort.Strings(files)

	var b strings.Builder
	totalSyms := 0
	var diagLines []string

	for _, rel := range files {
		abs, rerr := resolveRepoPath(repoRoot, rel)
		if rerr != nil {
			continue
		}
		src, rerr := os.ReadFile(abs)
		if rerr != nil || len(src) == 0 {
			continue
		}
		spans := fileSpans[rel]
		syms, _, perr := extractFileSymbolsCached(abs, rel, src)
		var touched []astSymbol
		if perr == nil {
			for _, s := range flattenSymbols(syms) {
				if spansOverlap(s.StartLine, s.EndLine, spans) {
					touched = append(touched, s)
				}
			}
		}
		fmt.Fprintf(&b, "\n### %s (%d hunk(s))\n", rel, len(spans))
		if len(touched) == 0 {
			b.WriteString("  (top-level / non-symbol lines changed)\n")
		} else {
			for _, s := range touched {
				totalSyms++
				fmt.Fprintf(&b, "- [%s] %s (L%d-L%d) — %s\n", s.Kind, s.NamePath, s.StartLine, s.EndLine, s.Signature)
				if incCallers && totalSyms <= 8 && len(s.Name) >= 3 {
					if callers := fastCallersViaRG(repoRoot, s, 3); len(callers) > 0 {
						for _, c := range callers {
							fmt.Fprintf(&b, "    caller: %s\n", strings.TrimPrefix(c, "- "))
						}
					}
				}
			}
		}
		if incDiags && len(diagLines) < 8 {
			if d := checkFileQuick(abs); d != "" {
				for _, dl := range strings.Split(d, "\n") {
					if dl != "" && len(diagLines) < 8 {
						diagLines = append(diagLines, dl)
					}
				}
			}
		}
	}

	header := fmt.Sprintf("Changed Symbols (%d files, %d affected symbols):\n", len(files), totalSyms)
	if len(diagLines) > 0 {
		b.WriteString("\n### Live Diagnostics on Modified Files\n")
		for _, dl := range diagLines {
			b.WriteString(dl + "\n")
		}
	}
	return truncateAnswer(strings.TrimSpace(header+b.String()), maxChars), nil
}
