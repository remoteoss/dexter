package parser

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestCallArity_MatchesElixir compares the arity Dexter reads at each named
// call with the arity Elixir's own parser gives it (testdata/
// call_arity_oracle.exs), over the Elixir sources in the test data and the
// hand-picked shapes in testdata/call_arity_cases.exs. DEXTER_ARITY_CORPUS
// adds more directories, such as a checkout of Elixir's own sources and
// tests. It needs elixir on PATH.
func TestCallArity_MatchesElixir(t *testing.T) {
	if testing.Short() {
		t.Skip("runs elixir")
	}
	elixir, err := exec.LookPath("elixir")
	if err != nil {
		t.Skip("elixir is not on PATH")
	}
	roots := []string{filepath.Join("testdata", "call_arity_cases.exs"), filepath.Join("..", "lsp", "testdata")}
	if extra := os.Getenv("DEXTER_ARITY_CORPUS"); extra != "" {
		roots = append(roots, filepath.SplitList(extra)...)
	}
	var files []string
	for _, root := range roots {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() && IsElixirFile(path) {
				files = append(files, path)
			}
			return nil
		})
	}
	cmd := exec.Command(elixir, filepath.Join("testdata", "call_arity_oracle.exs"))
	cmd.Stdin = strings.NewReader(strings.Join(files, "\n"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("oracle: %v\n%s", err, stderr.String())
	}

	type want struct {
		line, col, arity int
		name             string
	}
	byFile := map[string][]want{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
	for sc.Scan() {
		f := strings.Split(sc.Text(), "\t")
		if len(f) != 5 {
			continue
		}
		line, _ := strconv.Atoi(f[1])
		col, _ := strconv.Atoi(f[2])
		arity, _ := strconv.Atoi(f[4])
		byFile[f[0]] = append(byFile[f[0]], want{line, col, arity, f[3]})
	}

	if cases := filepath.Join("testdata", "call_arity_cases.exs"); len(byFile[cases]) == 0 {
		t.Fatalf("Elixir did not parse %s; oracle stderr:\n%s", cases, stderr.String())
	}
	checked, unknown := 0, 0
	mismatches := map[string][]string{} // a short shape key → examples
	for path, calls := range byFile {
		src, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		res := TokenizeFull(src)
		lines := bytes.Split(src, []byte("\n"))
		for _, c := range calls {
			if c.line-1 >= len(res.LineStarts) || c.line-1 >= len(lines) {
				continue
			}
			// Elixir counts columns in characters.
			text := lines[c.line-1]
			byteCol, n := 0, 1
			for n < c.col && byteCol < len(text) {
				_, size := utf8.DecodeRune(text[byteCol:])
				byteCol += size
				n++
			}
			offset := res.LineStarts[c.line-1] + byteCol
			got, found := -2, false
			if idx := TokenAtOffset(res.Tokens, offset); idx >= 0 && res.Tokens[idx].Start == offset && res.Tokens[idx].Kind == TokIdent && TokenText(src, res.Tokens[idx]) == c.name {
				got, found = callArity(src, res.Tokens, len(res.Tokens), idx), true
			} else if idx := TokenAtOffset(res.Interp, offset); idx >= 0 && res.Interp[idx].Start == offset && res.Interp[idx].Kind == TokIdent && TokenText(src, res.Interp[idx]) == c.name {
				got, found = InterpCallArity(src, res.InterpSpans, res.Interp[idx]), true
			}
			if !found || elixirKeyword[c.name] {
				// Not a name that Dexter records a call of: a keyword, an
				// operator (`and`, `in`), or a special form.
				continue
			}
			checked++
			if got == c.arity {
				continue
			}
			if got == -1 {
				unknown++
			}
			key := fmt.Sprintf("dexter %d, elixir %d", got, c.arity)
			mismatches[key] = append(mismatches[key], fmt.Sprintf("%s:%d: %s", path, c.line, strings.TrimSpace(string(text))))
		}
	}
	if checked == 0 {
		t.Fatalf("no call was checked; oracle stderr:\n%s", stderr.String())
	}
	keys := make([]string, 0, len(mismatches))
	total := 0
	for k, v := range mismatches {
		keys = append(keys, k)
		total += len(v)
	}
	sort.Slice(keys, func(i, j int) bool { return len(mismatches[keys[i]]) > len(mismatches[keys[j]]) })
	t.Logf("%d calls checked in %d files, %d differ (%d of them unknown to Dexter)", checked, len(byFile), total, unknown)
	for _, k := range keys {
		ex := mismatches[k]
		t.Errorf("%s: %d calls, for example:", k, len(ex))
		for i := 0; i < len(ex) && i < 4; i++ {
			t.Errorf("    %s", ex[i])
		}
	}
}
