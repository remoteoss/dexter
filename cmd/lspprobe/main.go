// Command lspprobe drives a dexter LSP server against a project on disk and
// prints what it answers. It is a development tool, not part of the shipped
// binary: `make build` compiles ./cmd only.
//
// It exists for on-the-spot checks against a real codebase, and for comparing
// two dexter builds against each other. Point it at any project; nothing about
// any particular codebase lives here.
//
// Probes are "<file>:<line>[:<column>]", with 1-based line and column as an
// editor shows them. A probe may instead name what to put the cursor on, which
// survives edits to the file:
//
//	lspprobe -root ~/code/my_app lib/my_app/accounts.ex:42:9
//	lspprobe -root ~/code/my_app 'lib/my_app/accounts.ex#get_user'
//	lspprobe -root ~/code/my_app -method references -json probes.txt
//
// Compare two builds on the same project:
//
//	lspprobe -binary ./a -root ~/p -json @probes.txt > a.json
//	lspprobe -binary ./b -root ~/p -json @probes.txt > b.json
//	diff a.json b.json
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/remoteoss/dexter/internal/lsptest"
)

type probe struct {
	Path   string `json:"path"`
	Line   int    `json:"line"`   // zero-based
	Column int    `json:"column"` // zero-based
}

type result struct {
	Probe      string   `json:"probe"`
	References []string `json:"references,omitempty"`
	RefCount   int      `json:"reference_count"`
	Definition []string `json:"definition,omitempty"`
	Hover      string   `json:"hover,omitempty"`
	Error      string   `json:"error,omitempty"`
	ElapsedMS  int64    `json:"elapsed_ms,omitempty"`
}

func main() {
	var (
		binary  = flag.String("binary", "dexter", "dexter binary to drive")
		root    = flag.String("root", ".", "project root the server indexes")
		method  = flag.String("method", "all", "references, definition, hover, or all")
		asJSON  = flag.Bool("json", false, "emit JSON (stable ordering, for diffing two builds)")
		counts  = flag.Bool("counts", false, "print only how many locations each probe returned")
		timeout = flag.Duration("timeout", 60*time.Second, "per-request timeout")
		settle  = flag.Duration("settle", 0, "wait after initialize before probing, to let background indexing and cache warm-up finish")
		verbose = flag.Bool("v", false, "pass the server's stderr through")
	)
	flag.Parse()

	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: lspprobe [flags] <file:line[:col] | file#needle | @probes-file>...")
		flag.PrintDefaults()
		os.Exit(2)
	}

	absRoot, err := filepath.Abs(*root)
	if err != nil {
		fatal(err)
	}

	probes, err := collectProbes(absRoot, flag.Args())
	if err != nil {
		fatal(err)
	}

	var stderr *os.File
	if *verbose {
		stderr = os.Stderr
	}
	client, err := lsptest.Start(*binary, absRoot, stderr)
	if err != nil {
		fatal(err)
	}
	defer client.Close()
	client.SetTimeout(*timeout)

	// Background work (reindex, cache warm-up) races a probe that fires the
	// instant the server is up. A real editor waits; -settle makes a
	// measurement comparable to one.
	if *settle > 0 {
		time.Sleep(*settle)
	}

	want := func(m string) bool { return *method == "all" || *method == m }

	results := make([]result, 0, len(probes))
	for _, p := range probes {
		rel, _ := filepath.Rel(absRoot, p.Path)
		r := result{Probe: fmt.Sprintf("%s:%d:%d", rel, p.Line+1, p.Column+1)}
		start := time.Now()

		if want("references") {
			locs, err := client.References(p.Path, p.Line, p.Column, false)
			if err != nil {
				r.Error = err.Error()
			} else {
				r.References = lsptest.Lines(absRoot, locs)
				r.RefCount = len(locs)
			}
		}
		if want("definition") && r.Error == "" {
			locs, err := client.Definition(p.Path, p.Line, p.Column)
			if err != nil {
				r.Error = err.Error()
			} else {
				r.Definition = lsptest.Lines(absRoot, locs)
			}
		}
		if want("hover") && r.Error == "" {
			text, err := client.Hover(p.Path, p.Line, p.Column)
			if err != nil {
				r.Error = err.Error()
			} else {
				r.Hover = text
			}
		}
		r.ElapsedMS = time.Since(start).Milliseconds()

		// Counts mode keeps output small enough to diff across thousands of
		// probes; the full location lists can run to tens of thousands of lines.
		if *counts {
			r.References, r.Definition, r.Hover = nil, nil, ""
		}
		results = append(results, r)
	}

	if *asJSON {
		// Timings are real but not reproducible, so they never reach a diff.
		for i := range results {
			results[i].ElapsedMS = 0
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", " ")
		if err := enc.Encode(results); err != nil {
			fatal(err)
		}
		return
	}

	for _, r := range results {
		fmt.Printf("%s  (%dms)\n", r.Probe, r.ElapsedMS)
		if r.Error != "" {
			fmt.Printf("  error: %s\n", r.Error)
			continue
		}
		if want("references") {
			fmt.Printf("  references: %d\n", r.RefCount)
			for _, l := range capped(r.References) {
				fmt.Printf("    %s\n", l)
			}
		}
		if want("definition") {
			for _, l := range r.Definition {
				fmt.Printf("  definition: %s\n", l)
			}
		}
		if want("hover") && r.Hover != "" {
			fmt.Printf("  hover: %s\n", strings.ReplaceAll(strings.TrimSpace(r.Hover), "\n", "\n         "))
		}
	}
}

// capped keeps terminal output readable when a probe matches thousands of sites.
func capped(lines []string) []string {
	const max = 20
	if len(lines) <= max {
		return lines
	}
	out := append([]string{}, lines[:max]...)
	return append(out, fmt.Sprintf("... and %d more", len(lines)-max))
}

// collectProbes expands each argument. "@file" reads one probe per line, so a
// large probe set lives in a file rather than on the command line.
func collectProbes(root string, args []string) ([]probe, error) {
	var out []probe
	for _, arg := range args {
		if strings.HasPrefix(arg, "@") {
			lines, err := readLines(arg[1:])
			if err != nil {
				return nil, err
			}
			for _, line := range lines {
				p, err := parseProbe(root, line)
				if err != nil {
					return nil, err
				}
				out = append(out, p)
			}
			continue
		}
		p, err := parseProbe(root, arg)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			lines = append(lines, line)
		}
	}
	return lines, scanner.Err()
}

// parseProbe accepts "file:line:col", "file:line", and "file#needle[#nth]".
func parseProbe(root, spec string) (probe, error) {
	resolve := func(p string) string {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(root, p)
	}

	if file, needle, found := strings.Cut(spec, "#"); found {
		nth := 1
		if n, rest, ok := strings.Cut(needle, "#"); ok {
			needle = n
			parsed, err := strconv.Atoi(rest)
			if err != nil {
				return probe{}, fmt.Errorf("probe %q: occurrence must be a number", spec)
			}
			nth = parsed
		}
		path := resolve(file)
		line, col, err := lsptest.Find(path, needle, nth)
		if err != nil {
			return probe{}, err
		}
		return probe{Path: path, Line: line, Column: col}, nil
	}

	parts := strings.Split(spec, ":")
	if len(parts) < 2 {
		return probe{}, fmt.Errorf("probe %q: want file:line[:col] or file#needle", spec)
	}
	line, err := strconv.Atoi(parts[1])
	if err != nil {
		return probe{}, fmt.Errorf("probe %q: bad line: %w", spec, err)
	}
	col := 1
	if len(parts) > 2 {
		if col, err = strconv.Atoi(parts[2]); err != nil {
			return probe{}, fmt.Errorf("probe %q: bad column: %w", spec, err)
		}
	}
	if line < 1 || col < 1 {
		return probe{}, fmt.Errorf("probe %q: line and column are 1-based", spec)
	}
	return probe{Path: resolve(parts[0]), Line: line - 1, Column: col - 1}, nil
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "lspprobe: %v\n", err)
	os.Exit(1)
}
