package saver

// RTK tool-output filters, ported from 9Router open-sse/rtk (itself a port of
// the Rust rtk tool). Copyright (c) 9Router contributors, MIT License (see
// LICENSE-9router). Lengths are bytes here (UTF-16 units in JS); the same for
// the ASCII tool output these filters target.

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	rawCap           = 10 << 20 // skip blobs over 10 MiB
	minCompressSize  = 500      // skip tiny blobs
	detectWindow     = 1024     // autodetect looks at the first N bytes
	diffHunkMax      = 100
	diffMaxLines     = 500
	gitLogMaxLines   = 200
	dedupLineMax     = 2000
	grepPerFile      = 10
	findPerDir       = 10
	findTotalDirs    = 20
	statusMaxFiles   = 10
	statusMaxUntrack = 10
	lsExtTop         = 5
	treeMaxLines     = 200
	searchPerDir     = 10
	searchTotalDirs  = 20
	truncHead        = 120
	truncTail        = 60
	truncMinLines    = 250
	readNumRatio     = 0.7
	deprecationKeep  = 3
)

type filter struct {
	name string
	fn   func(string) string
}

var (
	reGitDiff     = regexp.MustCompile(`(?m)^diff --git `)
	reGitDiffHunk = regexp.MustCompile(`(?m)^@@ `)
	reGitStatus   = regexp.MustCompile(`(?m)^On branch |^nothing to commit|^Changes (not |to be )|^Untracked files:`)
	reGitLog      = regexp.MustCompile(`(?m)^[*|/\\ ]*commit [0-9a-f]{7,40}$`)
	rePorcelain   = regexp.MustCompile(`^[ MADRCU?!][ MADRCU?!] \S`)
	reBuildOutput = regexp.MustCompile(`(?im)^(npm (warn|error|ERR!)|yarn (warn|error)|\s*Compiling\s+\S+|\s*Downloading\s+\S+|added \d+ package|\[ERROR\]|BUILD (SUCCESS|FAILED)|\s*Finished\s+|Successfully (installed|built)|ERROR:)`)
	reTreeGlyph   = regexp.MustCompile(`[├└]──|│  `)
	reLsRow       = regexp.MustCompile(`(?m)^[-dlbcps][rwx-]{9}`)
	reLsTotal     = regexp.MustCompile(`(?m)^total \d+$`)
	reSearchHead  = regexp.MustCompile(`^Result of search in '[^']*' \(total (\d+) files?\):`)
	reReadNum     = regexp.MustCompile(`^\s*\d+\|`)
	reDigits      = regexp.MustCompile(`^\d+$`)
	reDrive       = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

	fGitDiff     = filter{"git-diff", gitDiff}
	fGitStatus   = filter{"git-status", gitStatus}
	fGitLog      = filter{"git-log", gitLog}
	fBuildOutput = filter{"build-output", buildOutput}
	fGrep        = filter{"grep", grepFilter}
	fFind        = filter{"find", findFilter}
	fDedupLog    = filter{"dedup-log", dedupLog}
	fLs          = filter{"ls", lsFilter}
	fTree        = filter{"tree", treeFilter}
	fSmartTrunc  = filter{"smart-truncate", func(s string) string { return headTail(s, "") }}
	fReadNum     = filter{"read-numbered", func(s string) string { return headTail(s, " (file continues)") }}
	fSearchList  = filter{"search-list", searchList}
)

// detectFilter picks a filter from the start of text (nil: leave it alone).
func detectFilter(text string) *filter {
	head := text
	if len(head) > detectWindow {
		n := detectWindow
		for n > 0 && !utf8.RuneStart(head[n]) {
			n--
		}
		head = head[:n]
	}
	switch {
	case reGitLog.MatchString(head):
		return &fGitLog
	case reGitDiff.MatchString(head) || reGitDiffHunk.MatchString(head):
		return &fGitDiff
	case reGitStatus.MatchString(head):
		return &fGitStatus
	case reBuildOutput.MatchString(head):
		return &fBuildOutput
	case mostlyPorcelain(head):
		return &fGitStatus
	}
	lines := strings.Split(head, "\n")
	var nonEmpty []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			nonEmpty = append(nonEmpty, l)
		}
	}
	for i, l := range nonEmpty {
		if i == 5 {
			break
		}
		if isGrepLine(l) {
			return &fGrep
		}
	}
	if len(nonEmpty) >= 3 && all(nonEmpty, isPathLike) {
		return &fFind
	}
	switch {
	case reTreeGlyph.MatchString(head):
		return &fTree
	case reLsTotal.MatchString(head) || len(reLsRow.FindAllStringIndex(head, -1)) >= 3:
		return &fLs
	case reSearchHead.MatchString(head):
		return &fSearchList
	case len(lines) >= truncMinLines && lineNumbered(lines):
		return &fReadNum
	case len(nonEmpty) >= 5:
		return &fDedupLog
	case strings.Count(text, "\n")+1 >= truncMinLines:
		return &fSmartTrunc
	}
	return nil
}

// compressText runs the detected filter; ok=false when nothing was saved.
func compressText(text string) (string, string, bool) {
	if len(text) < minCompressSize || len(text) > rawCap {
		return text, "", false
	}
	f := detectFilter(text)
	if f == nil {
		return text, "", false
	}
	out := safeApply(f.fn, text)
	if out == "" || len(out) >= len(text) {
		return text, "", false
	}
	return out, f.name, true
}

func safeApply(fn func(string) string, text string) (out string) {
	defer func() {
		if recover() != nil {
			out = text
		}
	}()
	return fn(text)
}

func all(ls []string, f func(string) bool) bool {
	for _, l := range ls {
		if !f(l) {
			return false
		}
	}
	return true
}

func isGrepLine(line string) bool {
	first := strings.IndexByte(line, ':')
	if first < 0 {
		return false
	}
	second := strings.IndexByte(line[first+1:], ':')
	if second < 0 {
		return false
	}
	return reDigits.MatchString(line[first+1 : first+1+second])
}

func isPathLike(line string) bool {
	t := strings.TrimSpace(line)
	if t == "" {
		return false
	}
	if reDrive.MatchString(t) {
		return true
	}
	if strings.Contains(t, ":") {
		return false
	}
	return strings.HasPrefix(t, ".") || strings.HasPrefix(t, "/") || strings.Contains(t, "/")
}

func mostlyPorcelain(head string) bool {
	n, hits := 0, 0
	for _, l := range strings.Split(head, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		n++
		if rePorcelain.MatchString(l) {
			hits++
		}
	}
	return n >= 3 && float64(hits)/float64(n) >= 0.6
}

func lineNumbered(lines []string) bool {
	hits, nonEmpty := 0, 0
	for i, l := range lines {
		if i == 100 {
			break
		}
		if l == "" {
			continue
		}
		nonEmpty++
		if reReadNum.MatchString(l) {
			hits++
		}
	}
	return nonEmpty >= 5 && float64(hits)/float64(nonEmpty) >= readNumRatio
}

// ---- git diff (Rust git::compact_diff) ----

func gitDiff(diff string) string {
	var res []string
	currentFile := ""
	added, removed, hunkShown, hunkSkipped := 0, 0, 0, 0
	inHunk, truncated := false, false
	flushSkipped := func() {
		if hunkSkipped > 0 {
			res = append(res, fmt.Sprintf("  ... (%d lines truncated)", hunkSkipped))
			truncated = true
			hunkSkipped = 0
		}
	}
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git"):
			flushSkipped()
			if currentFile != "" && (added > 0 || removed > 0) {
				res = append(res, fmt.Sprintf("  +%d -%d", added, removed))
			}
			if parts := strings.Split(line, " b/"); len(parts) > 1 {
				currentFile = strings.Join(parts[1:], " b/")
			} else {
				currentFile = "unknown"
			}
			res = append(res, "\n"+currentFile)
			added, removed, inHunk, hunkShown = 0, 0, false, 0
		case strings.HasPrefix(line, "@@"):
			flushSkipped()
			inHunk, hunkShown = true, 0
			res = append(res, "  "+line)
		case inHunk:
			plus := strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++")
			minus := strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---")
			switch {
			case plus || minus:
				if plus {
					added++
				} else {
					removed++
				}
				if hunkShown < diffHunkMax {
					res = append(res, "  "+line)
					hunkShown++
				} else {
					hunkSkipped++
				}
			case hunkShown < diffHunkMax && !strings.HasPrefix(line, `\`) && hunkShown > 0:
				res = append(res, "  "+line)
				hunkShown++
			}
		}
		if len(res) >= diffMaxLines {
			res = append(res, "\n... (more changes truncated)")
			truncated = true
			break
		}
	}
	flushSkipped()
	if currentFile != "" && (added > 0 || removed > 0) {
		res = append(res, fmt.Sprintf("  +%d -%d", added, removed))
	}
	if truncated {
		res = append(res, "[full diff: rtk git diff --no-compact]")
	}
	return strings.Join(res, "\n")
}

// ---- git log ----

var (
	reLogCommit     = regexp.MustCompile(`(?i)^commit [0-9a-f]{7,40}$`)
	reLogCommitG    = regexp.MustCompile(`(?i)^[*|/\\ ]+commit [0-9a-f]{7,40}`)
	reLogAuthorDate = regexp.MustCompile(`(?i)^[*|/\\ ]*(Author|Date):`)
	reLogSubject    = regexp.MustCompile(`^[*|/\\ ]*    \S`)
	reLogStat       = regexp.MustCompile(`^\d+ file\w* changed`)
	reLogGraphSha   = regexp.MustCompile(`(?i)^[*|/\\ ]+([0-9a-f]{7,40}\s+.+)`)
	reLogOneline    = regexp.MustCompile(`^[0-9a-f]{7,40}\s+`)
	reLogGraphOnly  = regexp.MustCompile(`^[*|/\\ ]+$`)
)

func gitLog(input string) string {
	if input == "" {
		return ""
	}
	var out []string
	skipped := 0
	inCommit, subjectSeen := false, false
	push := func(l string) {
		if len(out) < gitLogMaxLines {
			out = append(out, l)
		} else {
			skipped++
		}
	}
	for _, raw := range strings.Split(input, "\n") {
		line := strings.TrimRight(raw, " \t\r\n\v\f")
		trimmed := strings.TrimSpace(line)
		if reLogCommit.MatchString(trimmed) || reLogCommitG.MatchString(trimmed) {
			inCommit, subjectSeen = true, false
			push(line)
			continue
		}
		if inCommit {
			switch {
			case reLogAuthorDate.MatchString(trimmed):
				push(trimmed)
			case trimmed == "":
			case !subjectSeen && reLogSubject.MatchString(line):
				push("  Subject: " + trimmed)
				subjectSeen = true
			case reLogStat.MatchString(trimmed):
				push("  " + trimmed)
			case strings.HasPrefix(trimmed, "diff --git "):
				push("  ... diff body omitted")
			}
			continue
		}
		if m := reLogGraphSha.FindStringSubmatch(trimmed); m != nil {
			push(m[1])
			continue
		}
		if reLogOneline.MatchString(trimmed) {
			push(trimmed)
			continue
		}
		if reLogGraphOnly.MatchString(trimmed) && strings.ContainsAny(trimmed, `*|/\`) {
			continue
		}
		push(trimmed)
	}
	if skipped > 0 {
		out = append(out, fmt.Sprintf("... (%d more lines)", skipped))
	}
	res := strings.Join(out, "\n")
	if res == "" || len(res) > len(input) {
		return input
	}
	return res
}

// ---- git status (Rust git::format_status_output) ----

var (
	reStatusBranch = regexp.MustCompile(`^On branch (\S+)`)
	reStatusLong   = regexp.MustCompile(`^\s*(modified|new file|deleted|renamed|both modified):\s+(.+)$`)
	reStatusShort  = regexp.MustCompile(`^[ MADRCU?!][ MADRCU?!] `)
)

func gitStatus(input string) string {
	lines := strings.Split(input, "\n")
	if len(lines) == 1 && strings.TrimSpace(lines[0]) == "" {
		return "Clean working tree"
	}
	branch := ""
	var stagedF, modifiedF, untrackedF []string
	staged, modified, untracked, conflicts := 0, 0, 0, 0
	for _, raw := range lines {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		if m := reStatusBranch.FindStringSubmatch(raw); m != nil {
			branch = m[1]
			continue
		}
		if strings.HasPrefix(raw, "##") {
			branch = strings.TrimLeft(raw[2:], " \t")
			continue
		}
		if len(raw) >= 3 && reStatusShort.MatchString(raw) {
			x, y, file := raw[0], raw[1], raw[3:]
			if raw[:2] == "??" {
				untracked++
				untrackedF = append(untrackedF, file)
				continue
			}
			if strings.IndexByte("MADRC", x) >= 0 {
				staged++
				stagedF = append(stagedF, file)
			} else if x == 'U' {
				conflicts++
			}
			if y == 'M' || y == 'D' {
				modified++
				modifiedF = append(modifiedF, file)
			}
			continue
		}
		if m := reStatusLong.FindStringSubmatch(raw); m != nil {
			path := strings.TrimSpace(m[2])
			switch m[1] {
			case "both modified":
				conflicts++
			case "modified", "deleted":
				modified++
				modifiedF = append(modifiedF, path)
			default:
				staged++
				stagedF = append(stagedF, path)
			}
		}
	}
	var b strings.Builder
	if branch != "" {
		fmt.Fprintf(&b, "* %s\n", branch)
	}
	list := func(label string, n int, files []string, max int) {
		if n == 0 {
			return
		}
		fmt.Fprintf(&b, "%s: %d files\n", label, n)
		for i, f := range files {
			if i == max {
				break
			}
			fmt.Fprintf(&b, "   %s\n", f)
		}
		if len(files) > max {
			fmt.Fprintf(&b, "   ... +%d more\n", len(files)-max)
		}
	}
	list("+ Staged", staged, stagedF, statusMaxFiles)
	list("~ Modified", modified, modifiedF, statusMaxFiles)
	list("? Untracked", untracked, untrackedF, statusMaxUntrack)
	if conflicts > 0 {
		fmt.Fprintf(&b, "conflicts: %d files\n", conflicts)
	}
	if staged == 0 && modified == 0 && untracked == 0 && conflicts == 0 {
		b.WriteString("clean — nothing to commit\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// ---- build output (npm, cargo, pip, maven, gradle) ----

var (
	reCargoCont   = regexp.MustCompile(`^\s*(-->|\||\d+\s*\||=)`)
	reNpmErr      = regexp.MustCompile(`(?i)^npm (ERR!|error)|^yarn error`)
	reNpmDeprec   = regexp.MustCompile(`(?i)^npm warn deprecated`)
	reNpmWarn     = regexp.MustCompile(`(?i)^npm warn|^yarn warn`)
	reRustErr     = regexp.MustCompile(`(?i)^error(\[|:)`)
	reRustWarn    = regexp.MustCompile(`(?i)^warning(\[|:)`)
	reErrColon    = regexp.MustCompile(`(?i)^ERROR:|^\[ERROR\]|^BUILD FAILED`)
	reMvnWarn     = regexp.MustCompile(`(?i)^\[WARNING\]`)
	reCompiling   = regexp.MustCompile(`(?i)^\s*Compiling\s+\S+`)
	reDownloading = regexp.MustCompile(`(?i)^\s*Downloading\s+\S+|^Fetching\s+`)
	reBuildSum    = regexp.MustCompile("(?i)^(added|removed|changed|audited|installed)\\s+\\d+\\s+package|^\\s*Finished\\s+|^BUILD SUCCESS|^\\d+\\s+(vulnerabilities|packages?|warnings?|errors?)|^Successfully (installed|built)|^To address .* issues|^Run `npm (audit|fund)`|packages are looking for funding")
)

func buildOutput(input string) string {
	var errs, warns, deps []string
	summary := ""
	compiling, downloading := 0, 0
	inCargo := false
	for _, line := range strings.Split(input, "\n") {
		t := strings.TrimSpace(line)
		if inCargo {
			if t == "" {
				inCargo = false
				continue
			}
			if reCargoCont.MatchString(line) {
				errs = append(errs, line)
				continue
			}
			inCargo = false
		}
		switch {
		case t == "":
		case reNpmErr.MatchString(t):
			errs = append(errs, line)
		case reNpmDeprec.MatchString(t):
			deps = append(deps, line)
		case reNpmWarn.MatchString(t):
			warns = append(warns, line)
		case reRustErr.MatchString(t) || strings.HasPrefix(t, "error -->"):
			errs = append(errs, line)
			inCargo = true
		case reRustWarn.MatchString(t) || strings.HasPrefix(t, "warning -->"):
			warns = append(warns, line)
			inCargo = true
		case reErrColon.MatchString(t):
			errs = append(errs, line)
		case reMvnWarn.MatchString(t):
			warns = append(warns, line)
		case reCompiling.MatchString(t):
			compiling++
		case reDownloading.MatchString(t):
			downloading++
		case reBuildSum.MatchString(t):
			if summary != "" {
				summary += "\n"
			}
			summary += line
		}
	}
	var b strings.Builder
	for i, d := range deps {
		if i == deprecationKeep {
			break
		}
		b.WriteString(d + "\n")
	}
	if len(deps) > deprecationKeep {
		fmt.Fprintf(&b, "... +%d more deprecated packages\n", len(deps)-deprecationKeep)
	}
	if compiling > 0 {
		fmt.Fprintf(&b, "Compiled %d packages\n", compiling)
	}
	if downloading > 0 {
		fmt.Fprintf(&b, "Downloaded %d packages\n", downloading)
	}
	for _, e := range errs {
		b.WriteString(e + "\n")
	}
	for i, w := range warns {
		if i == 5 {
			break
		}
		b.WriteString(w + "\n")
	}
	if len(warns) > 5 {
		fmt.Fprintf(&b, "... +%d more warnings\n", len(warns)-5)
	}
	if summary != "" {
		b.WriteString(summary + "\n")
	}
	if out := strings.TrimRight(b.String(), "\n"); out != "" {
		return out
	}
	return input
}

// ---- grep (file:line:content) ----

func grepFilter(input string) string {
	byFile := map[string][][2]string{}
	total := 0
	for _, line := range strings.Split(input, "\n") {
		first := strings.IndexByte(line, ':')
		if first < 0 {
			continue
		}
		second := strings.IndexByte(line[first+1:], ':')
		if second < 0 {
			continue
		}
		second += first + 1
		num := line[first+1 : second]
		if !reDigits.MatchString(num) {
			continue
		}
		total++
		f := line[:first]
		byFile[f] = append(byFile[f], [2]string{num, line[second+1:]})
	}
	if total == 0 {
		return input
	}
	files := sortedKeys(byFile)
	var b strings.Builder
	fmt.Fprintf(&b, "%d matches in %dF:\n\n", total, len(files))
	for _, f := range files {
		ms := byFile[f]
		fmt.Fprintf(&b, "[file] %s (%d):\n", f, len(ms))
		for i, m := range ms {
			if i == grepPerFile {
				break
			}
			fmt.Fprintf(&b, "  %4s: %s\n", m[0], strings.TrimSpace(m[1]))
		}
		if len(ms) > grepPerFile {
			fmt.Fprintf(&b, "  +%d\n", len(ms)-grepPerFile)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---- find (paths grouped by directory) ----

func findFilter(input string) string {
	var lines []string
	for _, l := range strings.Split(input, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return input
	}
	byDir := map[string][]string{}
	for _, p := range lines {
		sep := max(strings.LastIndexByte(p, '/'), strings.LastIndexByte(p, '\\'))
		dir, base := ".", p
		if sep >= 0 {
			dir, base = p[:sep], p[sep+1:]
			if dir == "" {
				dir = "/"
			}
		}
		byDir[dir] = append(byDir[dir], base)
	}
	dirs := sortedKeys(byDir)
	var b strings.Builder
	fmt.Fprintf(&b, "%d files in %d dirs:\n\n", len(lines), len(dirs))
	for i, d := range dirs {
		if i == findTotalDirs {
			break
		}
		files := byDir[d]
		fmt.Fprintf(&b, "%s/  (%d)\n", strings.ReplaceAll(d, `\`, "/"), len(files))
		for j, f := range files {
			if j == findPerDir {
				break
			}
			fmt.Fprintf(&b, "  %s\n", f)
		}
		if len(files) > findPerDir {
			fmt.Fprintf(&b, "  +%d\n", len(files)-findPerDir)
		}
	}
	if len(dirs) > findTotalDirs {
		fmt.Fprintf(&b, "\n+%d more dirs\n", len(dirs)-findTotalDirs)
	}
	return b.String()
}

// ---- dedup log (collapse repeats and blank runs) ----

func dedupLog(input string) string {
	var out []string
	prev, havePrev := "", false
	run, blanks := 0, 0
	flush := func() {
		if havePrev && run > 1 {
			out = append(out, fmt.Sprintf("  ... (%d duplicate lines)", run-1))
		}
	}
	for _, line := range strings.Split(input, "\n") {
		if strings.TrimSpace(line) == "" {
			if blanks < 1 {
				out = append(out, line)
			}
			blanks++
			flush()
			havePrev, run = false, 0
			continue
		}
		blanks = 0
		if havePrev && line == prev {
			run++
			continue
		}
		flush()
		out = append(out, line)
		prev, havePrev, run = line, true, 1
		if len(out) >= dedupLineMax {
			out = append(out, fmt.Sprintf("... (truncated at %d lines)", dedupLineMax))
			return strings.Join(out, "\n")
		}
	}
	flush()
	return strings.Join(out, "\n")
}

// ---- ls -la (Rust compact_ls) ----

var (
	reLsDate    = regexp.MustCompile(`\s+(Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)\s+\d{1,2}\s+(\d{4}|\d{2}:\d{2})\s+`)
	lsNoiseDirs = map[string]bool{}
)

func init() {
	for _, d := range []string{"node_modules", ".git", "target", "__pycache__", ".next", "dist", "build", ".cache", ".turbo",
		".vercel", ".pytest_cache", ".mypy_cache", ".tox", ".venv", "venv", "env", "coverage", ".nyc_output", ".DS_Store",
		"Thumbs.db", ".idea", ".vscode", ".vs", "*.egg-info", ".eggs"} {
		lsNoiseDirs[d] = true
	}
}

func humanSize(n int) string {
	switch {
	case n >= 1048576:
		return fmt.Sprintf("%.1fM", float64(n)/1048576)
	case n >= 1024:
		return fmt.Sprintf("%.1fK", float64(n)/1024)
	}
	return fmt.Sprintf("%dB", n)
}

func lsFilter(input string) string {
	var dirs []string
	var files [][2]string
	var extOrder []string
	extCount := map[string]int{}
	for _, line := range strings.Split(input, "\n") {
		if strings.HasPrefix(line, "total ") || line == "" {
			continue
		}
		loc := reLsDate.FindStringIndex(line)
		if loc == nil {
			continue
		}
		name := line[loc[1]:]
		parts := strings.Fields(line[:loc[0]])
		if len(parts) < 4 || name == "." || name == ".." || lsNoiseDirs[name] {
			continue
		}
		size := 0
		for i := len(parts) - 1; i >= 0; i-- {
			if n, err := strconv.Atoi(parts[i]); err == nil && strconv.Itoa(n) == parts[i] {
				size = n
				break
			}
		}
		switch parts[0][0] {
		case 'd':
			dirs = append(dirs, name)
		case '-', 'l':
			ext := "no ext"
			if dot := strings.LastIndexByte(name, '.'); dot > 0 {
				ext = name[dot:]
			}
			if extCount[ext] == 0 {
				extOrder = append(extOrder, ext)
			}
			extCount[ext]++
			files = append(files, [2]string{name, humanSize(size)})
		}
	}
	if len(dirs) == 0 && len(files) == 0 {
		return input
	}
	var b strings.Builder
	for _, d := range dirs {
		b.WriteString(d + "/\n")
	}
	for _, f := range files {
		fmt.Fprintf(&b, "%s  %s\n", f[0], f[1])
	}
	fmt.Fprintf(&b, "\nSummary: %d files, %d dirs", len(files), len(dirs))
	if len(extOrder) > 0 {
		sort.SliceStable(extOrder, func(i, j int) bool { return extCount[extOrder[i]] > extCount[extOrder[j]] })
		var ps []string
		for i, e := range extOrder {
			if i == lsExtTop {
				break
			}
			ps = append(ps, fmt.Sprintf("%d %s", extCount[e], e))
		}
		b.WriteString(" (" + strings.Join(ps, ", "))
		if len(extOrder) > lsExtTop {
			fmt.Fprintf(&b, ", +%d more", len(extOrder)-lsExtTop)
		}
		b.WriteString(")")
	}
	return b.String()
}

// ---- tree ----

func treeFilter(input string) string {
	var kept []string
	for _, line := range strings.Split(input, "\n") {
		if strings.Contains(line, "director") && strings.Contains(line, "file") {
			continue
		}
		if strings.TrimSpace(line) == "" && len(kept) == 0 {
			continue
		}
		kept = append(kept, line)
	}
	for len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
		kept = kept[:len(kept)-1]
	}
	if len(kept) > treeMaxLines {
		return strings.Join(kept[:treeMaxLines], "\n") + fmt.Sprintf("\n... +%d more lines", len(kept)-treeMaxLines)
	}
	return strings.Join(kept, "\n")
}

// ---- head + tail truncation (smart-truncate and read-numbered) ----

func headTail(input, note string) string {
	lines := strings.Split(input, "\n")
	if len(lines) < truncMinLines {
		return input
	}
	cut := len(lines) - truncHead - truncTail
	out := append([]string{}, lines[:truncHead]...)
	out = append(out, fmt.Sprintf("... +%d lines truncated%s", cut, note))
	return strings.Join(append(out, lines[len(lines)-truncTail:]...), "\n")
}

// ---- Cursor glob result list ----

func searchList(input string) string {
	lines := strings.Split(input, "\n")
	var paths []string
	for _, raw := range lines[1:] {
		if t := strings.TrimSpace(raw); strings.HasPrefix(t, "- ") {
			paths = append(paths, t[2:])
		}
	}
	if len(paths) == 0 {
		return input
	}
	byDir := map[string][]string{}
	for _, p := range paths {
		dir, name := ".", p
		if s := strings.LastIndexByte(p, '/'); s >= 0 {
			dir, name = p[:s], p[s+1:]
			if dir == "" {
				dir = "/"
			}
		}
		byDir[dir] = append(byDir[dir], name)
	}
	dirs := sortedKeys(byDir)
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n%d files in %d dirs:\n\n", lines[0], len(paths), len(dirs))
	for i, d := range dirs {
		if i == searchTotalDirs {
			break
		}
		names := byDir[d]
		fmt.Fprintf(&b, "%s/ (%d):\n", d, len(names))
		for j, n := range names {
			if j == searchPerDir {
				break
			}
			fmt.Fprintf(&b, "  %s\n", n)
		}
		if len(names) > searchPerDir {
			fmt.Fprintf(&b, "  +%d\n", len(names)-searchPerDir)
		}
		b.WriteString("\n")
	}
	if len(dirs) > searchTotalDirs {
		fmt.Fprintf(&b, "+%d more dirs\n", len(dirs)-searchTotalDirs)
	}
	return strings.TrimRight(b.String(), "\n")
}
