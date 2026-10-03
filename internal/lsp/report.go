package lsp

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/remoteoss/dexter/internal/notify"
)

// Condition keys. A key names one state; setting it again does not send a new
// message, and clearing it tells the user that the state stopped. Keys that
// start with "index." describe the index, and the CLI shows them too.
const (
	// CondIndexBuild is the first build of an empty index.
	CondIndexBuild = "index.build"
	// CondIndexRebuild is a rebuild of an index that Dexter had to delete: it
	// was written by another build, or it could not be opened.
	CondIndexRebuild = "index.rebuild"
	// CondIndexUnavailable is an index that Dexter cannot use at all.
	CondIndexUnavailable = "index.unavailable"
	// CondIndexFallback is a fast full build that failed, so the files are
	// indexed one by one.
	CondIndexFallback = "index.fallback"
	// CondIndexFiles is a set of files that could not be indexed.
	CondIndexFiles = "index.files"

	condStdlib    = "stdlib"
	condFormatter = "formatter"
	condOTP       = "formatter.otp"
)

// IndexConditionPrefix starts the key of every condition that describes the
// index.
const IndexConditionPrefix = "index."

// reconcileProgressThreshold is how many changed files an incremental pass
// updates before it shows progress. A save or a small branch switch stays
// silent; a large one, which can take minutes, does not.
const reconcileProgressThreshold = 1000

// Reporter returns the reporter that every session of this workspace shares.
func (c *IndexCoordinator) Reporter() *notify.Reporter { return c.reporter }

// fileFailures is the set of files that could not be indexed. Each file is
// told to the user only as a part of one aggregate message.
type fileFailures struct {
	count   atomic.Int32 // len(paths), read without the lock on the hot path
	changed atomic.Bool  // the set changed since the last report
	mu      sync.Mutex
	paths   map[string]string // path → error text
}

// fail records a file that could not be read, parsed, or written. A file that
// went away in the middle of the work is not a failure.
func (f *fileFailures) fail(path string, err error) {
	if errors.Is(err, fs.ErrNotExist) {
		f.ok(path)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.paths == nil {
		f.paths = make(map[string]string)
	}
	if _, ok := f.paths[path]; !ok {
		f.changed.Store(true)
	}
	f.paths[path] = err.Error()
	f.count.Store(int32(len(f.paths)))
}

// ok records a file that was indexed or removed. It costs one atomic load when
// no file has failed, which is the usual case.
func (f *fileFailures) ok(path string) {
	if f.count.Load() == 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, found := f.paths[path]; found {
		delete(f.paths, path)
		f.changed.Store(true)
		f.count.Store(int32(len(f.paths)))
	}
}

// removed drops the failures at path and below it. A file that failed was
// never stored, so removing a directory from the index does not list it; the
// prefix finds it anyway.
func (f *fileFailures) removed(path string) {
	if f.count.Load() == 0 {
		return
	}
	prefix := strings.TrimSuffix(path, string(filepath.Separator)) + string(filepath.Separator)
	f.mu.Lock()
	defer f.mu.Unlock()
	for failed := range f.paths {
		if failed == path || strings.HasPrefix(failed, prefix) {
			delete(f.paths, failed)
			f.changed.Store(true)
		}
	}
	f.count.Store(int32(len(f.paths)))
}

// retain drops failures for files that a full walk did not see: they are gone
// or no longer belong to the workspace.
func (f *fileFailures) retain(seen map[string]struct{}) {
	if f.count.Load() == 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for path := range f.paths {
		if _, ok := seen[path]; !ok {
			delete(f.paths, path)
			f.changed.Store(true)
		}
	}
	f.count.Store(int32(len(f.paths)))
}

// reportFileFailures tells the user when the set of failed files changed since
// the last report. When it did not change, which is the usual case, it costs
// one atomic load.
func (c *IndexCoordinator) reportFileFailures() {
	f := &c.failures
	if !f.changed.Load() {
		return
	}
	f.mu.Lock()
	if !f.changed.Swap(false) {
		f.mu.Unlock()
		return
	}
	paths := make([]string, 0, len(f.paths))
	for path := range f.paths {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	firstErr := ""
	if len(paths) > 0 {
		firstErr = f.paths[paths[0]]
	}
	f.mu.Unlock()

	if len(paths) == 0 {
		c.reporter.Clear(CondIndexFiles, "Dexter: all files that could not be indexed are indexed now.")
		return
	}
	noun := "files"
	if len(paths) == 1 {
		noun = "file"
	}
	c.reporter.Set(CondIndexFiles, notify.Warning, fmt.Sprintf(
		"Dexter: %d %s could not be indexed: %s (%s); see the log. Navigation does not find their definitions until Dexter can index them. Dexter tries again when they change.",
		len(paths), noun, notify.Summarize(paths), firstErr))
}

// beginIndexBuild tells the user that an empty index is being built. A rebuild
// already has its own condition, which says why; a first build gets an Info
// message, as before. A workspace with no Elixir files stays empty, so every
// full pass over it starts as a cold build: it is told only once for each
// workspace, and later passes return a nil task, which reports nothing.
func (s *Server) beginIndexBuild() *notify.Task {
	r := s.index.reporter
	first := s.index.firstBuildReported.CompareAndSwap(false, true)
	if r.Active(CondIndexRebuild) {
		return r.Begin(CondIndexBuild, "Dexter: building the index", "parsing every Elixir file in the project", false)
	}
	if !first {
		return nil
	}
	r.Set(CondIndexBuild, notify.Info, "Dexter: building the index for the first time; go-to-definition will be available shortly.")
	return r.Begin(CondIndexBuild, "Dexter: building the index", "parsing every Elixir file in the project", false)
}

// indexBuildFailedUnindexed tells the user that the bulk build left an index
// that Dexter cannot use.
func (s *Server) indexBuildFailedUnindexed(task *notify.Task, err error) {
	r := s.index.reporter
	task.End("Dexter: the index could not be completed")
	r.Clear(CondIndexBuild, "")
	r.Clear(CondIndexRebuild, "")
	r.Set(CondIndexUnavailable, notify.Error, fmt.Sprintf(
		"Dexter: the index could not be completed (%v), so navigation, references, and completion from the index do not work. Close the editors on this project, run `dexter init --force` in the project root, then open the editor again. If it happens again, please report it.", err))
}

// indexBuildFellBack tells the user that the fast build failed and the slow
// path is used.
func (s *Server) indexBuildFellBack(err error) {
	s.index.reporter.Set(CondIndexFallback, notify.Warning, fmt.Sprintf(
		"Dexter: the fast index build failed (%v). Dexter now indexes the files one by one, which is slower; navigation is limited until it ends. See the log for details.", err))
}

// finishReindex ends the reports of one full-or-incremental pass. The clear
// messages say that the index works fully again.
func (s *Server) finishReindex(task *notify.Task, files int, elapsed time.Duration) {
	r := s.index.reporter
	built := fmt.Sprintf("Dexter: index built (%d files in %s).", files, elapsed)
	task.End(built)
	r.Clear(CondIndexFallback, "")
	if r.Clear(CondIndexRebuild, fmt.Sprintf("Dexter: the index rebuild is complete (%d files in %s); navigation works fully again.", files, elapsed)) {
		r.Clear(CondIndexBuild, "")
	} else {
		r.Clear(CondIndexBuild, built)
	}
	s.index.reportFileFailures()
}

// reconcileProgress shows progress for an incremental pass that turns out to
// be large. It starts only after reconcileProgressThreshold changed files, so
// the usual pass costs one comparison per changed file.
type reconcileProgress struct {
	reporter *notify.Reporter
	task     *notify.Task
	files    int
	last     time.Time
}

func (p *reconcileProgress) file() {
	p.files++
	if p.files < reconcileProgressThreshold {
		return
	}
	if p.task == nil {
		p.task = p.reporter.Begin("index.reconcile", "Dexter: updating the index",
			fmt.Sprintf("%d changed files so far", p.files), true)
		p.last = time.Now()
		return
	}
	if p.files%100 == 0 && time.Since(p.last) >= time.Second {
		p.last = time.Now()
		p.task.Report(fmt.Sprintf("%d changed files so far", p.files), -1)
	}
}

func (p *reconcileProgress) end(elapsed time.Duration) {
	if p.task != nil {
		p.task.End(fmt.Sprintf("Dexter: index updated (%d changed files in %s).", p.files, elapsed))
	}
}

// ReportStdlib tells the user whether the workspace has an Elixir standard
// library. It reads the root that every session shares, not what one session
// found: one editor can pass stdlibPath while another has no way to find it,
// and the second must not report a library that the workspace already has.
func (s *Server) ReportStdlib() {
	r := s.index.reporter
	if root := s.StdlibRoot(); root != "" {
		r.Clear(condStdlib, fmt.Sprintf("Dexter: found the Elixir standard library at %s; navigation into it works now.", root))
		return
	}
	r.Set(condStdlib, notify.Warning, "Dexter: could not find the Elixir standard library, so standard library modules (Enum, String, and so on) do not resolve. Make sure that the Elixir version in .tool-versions or mise.toml is installed (for example `mise install`), or set DEXTER_ELIXIR_LIB_ROOT or the stdlibPath initialization option, then restart the editor.")
}

// mixMissingMessage tells one editor that its session has no mix binary. Each
// session finds mix for itself and uses it for its own formatting, so the
// report goes to that editor only.
const mixMissingMessage = "Dexter: could not find the `mix` binary, so formatting does not work in this editor. Install the Elixir version of this project (for example `mise install`) or put mix on the PATH of the editor, then restart the editor."

// otpMismatchRetry is how long a build root whose BEAM failed with an OTP
// mismatch uses mix format before Dexter tries the BEAM again, when nothing
// that can fix the mismatch changed first. A variable so tests can shrink it.
var otpMismatchRetry = 10 * time.Minute

// otpMismatch remembers a BEAM that failed with an OTP mismatch.
type otpMismatch struct {
	at    time.Time
	stamp string
}

// otpStamp identifies what can fix an OTP mismatch for a build root: the
// Elixir and mix binaries, and the _build directory.
func (s *Server) otpStamp(buildRoot string) string {
	elixir := filepath.Join(filepath.Dir(s.mixBin), "elixir")
	return fmt.Sprint(statFileStamp(elixir), statFileStamp(s.mixBin), statFileStamp(filepath.Join(buildRoot, "_build")))
}

// errOTPMismatch marks a mix format that failed with an OTP mismatch.
var errOTPMismatch = errors.New("Elixir/OTP version mismatch")

// rememberOTPMismatch records that the BEAM of buildRoot failed with an OTP
// mismatch, so that it is not started again on each save. It reports nothing:
// what the mismatch means for the user depends on the mix format fallback,
// and reportBeamOTP decides that.
func (s *Server) rememberOTPMismatch(buildRoot string) {
	s.beamMu.Lock()
	defer s.beamMu.Unlock()
	if s.otpMismatches == nil {
		s.otpMismatches = make(map[string]otpMismatch)
	}
	if _, ok := s.otpMismatches[buildRoot]; ok {
		return
	}
	s.otpMismatches[buildRoot] = otpMismatch{at: time.Now(), stamp: s.otpStamp(buildRoot)}
}

// reportBeamOTP tells the user about an OTP mismatch of the BEAM of buildRoot
// after a mix format fallback ran with result err. Only one of two
// conditions may show: when the fallback worked, a Warning that formatting is
// only slower; when the fallback failed with the same mismatch, the Error from
// reportFormatFailure alone, so this Warning is cleared without a message.
// Any other fallback failure, such as a syntax error, changes nothing.
func (s *Server) reportBeamOTP(buildRoot string, err error) {
	s.beamMu.Lock()
	holds := s.otpMismatchHolds(buildRoot)
	s.beamMu.Unlock()
	key := condOTP + ":" + buildRoot
	switch {
	case !holds:
		return
	case err == nil:
		s.index.reporter.Set(key, notify.Warning, fmt.Sprintf(
			"Dexter: Elixir/OTP version mismatch in %s: the Elixir install of this project was compiled for a newer OTP version than the one that runs, so the fast persistent formatter cannot start. Formatting still works through the slower `mix format` fallback. To fix it, update Erlang to match, or switch to an Elixir build that targets your current OTP (for example elixir@...-otp-27). Dexter tries the fast formatter again when the Elixir install or the _build directory changes, or after %s.",
			buildRoot, otpMismatchRetry))
	case errors.Is(err, errOTPMismatch):
		s.index.reporter.Clear(key, "")
	}
}

// otpMismatchHolds reports whether the BEAM of buildRoot failed with an OTP
// mismatch that nothing has fixed since. The caller holds beamMu.
func (s *Server) otpMismatchHolds(buildRoot string) bool {
	m, ok := s.otpMismatches[buildRoot]
	if !ok {
		return false
	}
	if time.Since(m.at) < otpMismatchRetry && m.stamp == s.otpStamp(buildRoot) {
		return true
	}
	delete(s.otpMismatches, buildRoot)
	return false
}

// reportBeamFormatWorks clears the formatter conditions after the persistent
// BEAM formatted a file. Only this ends an OTP mismatch of the build root: a
// mix format fallback works around the mismatch and does not fix it.
//
// Conditions are kept for each project: in an umbrella or a monorepo, one
// project can fail to format while another works, and a success in one must
// not clear, or set again, the report of the other.
func (s *Server) reportBeamFormatWorks(mixRoot, buildRoot string) {
	r := s.index.reporter
	formatter := r.Clear(condFormatter+":"+mixRoot, "")
	if r.Clear(condOTP+":"+buildRoot, "") {
		r.Notify(notify.Info, fmt.Sprintf("Dexter: the fast persistent formatter works again in %s.", buildRoot))
		return
	}
	if formatter {
		r.Notify(notify.Info, fmt.Sprintf("Dexter: formatting works again in %s.", mixRoot))
	}
}

// reportMixFormatWorks clears the report that formatting does not work in
// mixRoot after a mix format succeeded there.
func (s *Server) reportMixFormatWorks(mixRoot string) {
	s.index.reporter.Clear(condFormatter+":"+mixRoot, fmt.Sprintf("Dexter: formatting works again in %s.", mixRoot))
}

// reportFormatFailure tells the user that formatting cannot run in one Mix
// project. A syntax error in the user's code is not a failure of Dexter: it
// already shows as a diagnostic, or mix reports it.
func (s *Server) reportFormatFailure(mixRoot string, err error, stderr string) {
	if isUserCodeFormatError(stderr) {
		return
	}
	if isOTPMismatch(stderr) {
		s.index.reporter.Set(condFormatter+":"+mixRoot, notify.Error, fmt.Sprintf(
			"Dexter: formatting does not work in %s: Elixir/OTP version mismatch. The Elixir install of this project was compiled for a newer OTP version than the one that runs. Update Erlang to match, or switch to an Elixir build that targets your current OTP (for example elixir@...-otp-27).", mixRoot))
		return
	}
	detail := err.Error()
	if line := firstErrorLine(stderr); line != "" {
		detail = line
	}
	s.index.reporter.Set(condFormatter+":"+mixRoot, notify.Warning, fmt.Sprintf(
		"Dexter: formatting does not work in %s: `mix format` failed (%s). See the log for the full output. Make sure that the project compiles and that its formatter plugins are installed (`mix deps.get`).", mixRoot, detail))
}

// isUserCodeFormatError reports whether mix format failed because of the code
// it had to format.
func isUserCodeFormatError(stderr string) bool {
	for _, marker := range []string{"SyntaxError", "TokenMissingError", "MismatchedDelimiterError"} {
		if strings.Contains(stderr, marker) {
			return true
		}
	}
	return false
}

func firstErrorLine(stderr string) string {
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			if len(line) > 200 {
				line = line[:200] + "..."
			}
			return line
		}
	}
	return ""
}

// renameFailures collects the files that one rename could not change. Writes
// run in parallel, so it is safe for concurrent use.
type renameFailures struct {
	mu    sync.Mutex
	paths []string
	first string
}

func (f *renameFailures) add(path string, err error) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.paths) == 0 {
		f.first = err.Error()
	}
	f.paths = append(f.paths, path)
}

// report tells this editor, and only this editor, that its rename is
// incomplete.
func (s *Server) reportRenameFailures(f *renameFailures) {
	f.mu.Lock()
	paths := append([]string(nil), f.paths...)
	first := f.first
	f.mu.Unlock()
	if len(paths) == 0 {
		return
	}
	sort.Strings(paths)
	s.notifySession(notify.Error, fmt.Sprintf(
		"Dexter: the rename could not change %d files: %s (%s). These files still use the old name; see the log, then fix them by hand or undo the rename.",
		len(paths), notify.Summarize(paths), first))
}
