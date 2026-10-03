// Package notify is the one path by which Dexter tells the user that it does
// not work, that it works with less, or that it does long work.
//
// A Reporter writes each report to the log, as before, and also sends it to
// every attached editor: failures and degraded states as window/showMessage,
// long work as LSP work-done progress. The workspace daemon serves several
// editors, so the Reporter keeps the conditions that are active now and the
// work that is in progress, and it replays them to an editor that attaches
// later.
//
// Reports are made only when a state changes. Nothing here is on a request
// path, and delivery never blocks the caller: each editor has its own queue
// and its own goroutine.
package notify

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"go.lsp.dev/protocol"
)

// Severity is how bad a condition is for the user.
type Severity int

const (
	// Info is a normal state change, for example "the index is built".
	Info Severity = iota + 1
	// Warning is a degraded state: Dexter works, but with less.
	Warning
	// Error is a state in which Dexter does not work.
	Error
)

func (s Severity) String() string {
	switch s {
	case Error:
		return "error"
	case Warning:
		return "warning"
	default:
		return "info"
	}
}

func (s Severity) messageType() protocol.MessageType {
	switch s {
	case Error:
		return protocol.MessageTypeError
	case Warning:
		return protocol.MessageTypeWarning
	default:
		return protocol.MessageTypeInfo
	}
}

func (s Severity) logPrefix() string {
	switch s {
	case Error:
		return "Error: "
	case Warning:
		return "Warning: "
	default:
		return ""
	}
}

// Condition is one active state that the user must know about.
type Condition struct {
	Key      string
	Severity Severity
	Message  string
}

// deliveryTimeout bounds one request to an editor, such as
// window/workDoneProgress/create. An editor that does not answer must not stop
// the queue for ever.
const deliveryTimeout = 5 * time.Second

// maxQueue is the number of undelivered reports one editor can have. Reports
// are made only on state changes, so a full queue means that the editor does
// not read; progress reports are then dropped first.
const maxQueue = 256

// Reporter holds the active conditions and the work in progress for one
// workspace, and the editors attached to it. A nil Reporter discards reports,
// so code that has none needs no checks.
type Reporter struct {
	mu    sync.Mutex
	logf  func(format string, args ...any)
	seq   uint64
	conds map[string]*entry
	tasks map[string]*Task
	sinks map[*sink]struct{}
}

type entry struct {
	Condition
	seq uint64
}

// New returns a Reporter that logs through the standard logger.
func New() *Reporter {
	return &Reporter{
		logf:  log.Printf,
		conds: make(map[string]*entry),
		tasks: make(map[string]*Task),
		sinks: make(map[*sink]struct{}),
	}
}

// SetLogf replaces the log function. Tests use it to keep their output quiet or
// to record what is logged.
func (r *Reporter) SetLogf(logf func(format string, args ...any)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.logf = logf
	r.mu.Unlock()
}

func (r *Reporter) log(sev Severity, message string) {
	r.logLine("%s%s", sev.logPrefix(), strings.TrimPrefix(message, "Dexter: "))
}

func (r *Reporter) logLine(format string, args ...any) {
	if r.logf != nil {
		r.logf(format, args...)
	}
}

// Set makes a condition active, logs it, and shows it in every attached editor.
// A condition that is already active with the same severity only takes the new
// message, which later editors see: this keeps a count that changes from
// sending a message each time.
func (r *Reporter) Set(key string, sev Severity, message string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.conds[key]; ok && existing.Severity == sev {
		existing.Message = message
		return
	}
	r.seq++
	r.conds[key] = &entry{Condition: Condition{Key: key, Severity: sev, Message: message}, seq: r.seq}
	r.log(sev, message)
	r.broadcast(op{kind: opShow, sev: sev, message: message})
}

// Clear ends an active condition. A message that is not empty tells the user
// that the condition stopped; it is logged and shown as Info. Clear reports
// whether the condition was active.
func (r *Reporter) Clear(key, message string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.conds[key]; !ok {
		return false
	}
	delete(r.conds, key)
	if message != "" {
		r.log(Info, message)
		r.broadcast(op{kind: opShow, sev: Info, message: message})
	}
	return true
}

// Active reports whether a condition is active.
func (r *Reporter) Active(key string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.conds[key]
	return ok
}

// Notify logs one event and shows it in every attached editor. It is not kept,
// so an editor that attaches later does not see it. Use Set for a state that
// continues.
func (r *Reporter) Notify(sev Severity, message string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.log(sev, message)
	r.broadcast(op{kind: opShow, sev: sev, message: message})
}

// Conditions returns the active conditions, oldest first.
func (r *Reporter) Conditions() []Condition {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entries := make([]*entry, 0, len(r.conds))
	for _, e := range r.conds {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].seq < entries[j].seq })
	out := make([]Condition, len(entries))
	for i, e := range entries {
		out[i] = e.Condition
	}
	return out
}

// Task is one piece of long work, shown as LSP work-done progress.
type Task struct {
	r        *Reporter
	key      string
	token    string
	title    string
	message  string
	percent  int
	fallback bool
	ended    bool
	seq      uint64
}

// Begin starts long work and shows it in every attached editor that supports
// work-done progress. When fallback is true, an editor without that support
// gets a window/showMessage at the start and at the end instead; pass false when
// a condition already tells the user about the same work. A second Begin with a
// key that is in progress returns the task that is in progress. Progress is not
// logged: the caller logs the outcome, or a condition does.
func (r *Reporter) Begin(key, title, message string, fallback bool) *Task {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.tasks[key]; ok {
		return t
	}
	r.seq++
	t := &Task{
		r:        r,
		key:      key,
		token:    fmt.Sprintf("dexter/%s/%d", key, r.seq),
		title:    title,
		message:  message,
		percent:  -1,
		fallback: fallback,
		seq:      r.seq,
	}
	r.tasks[key] = t
	r.broadcast(t.beginOp())
	return t
}

func (t *Task) beginOp() op {
	return op{kind: opBegin, token: t.token, title: t.title, message: t.message, percent: t.percent, fallback: t.fallback}
}

// Report updates the message and, when percent is 0 to 100, the percentage of
// the work. Callers report at most a few times a second.
func (t *Task) Report(message string, percent int) {
	if t == nil {
		return
	}
	r := t.r
	r.mu.Lock()
	defer r.mu.Unlock()
	if t.ended || (message == t.message && percent == t.percent) {
		return
	}
	t.message = message
	t.percent = percent
	r.broadcast(op{kind: opReport, token: t.token, message: message, percent: percent})
}

// End finishes the work. The message tells the outcome.
func (t *Task) End(message string) {
	if t == nil {
		return
	}
	r := t.r
	r.mu.Lock()
	defer r.mu.Unlock()
	if t.ended {
		return
	}
	t.ended = true
	delete(r.tasks, t.key)
	r.broadcast(op{kind: opEnd, token: t.token, message: message, fallback: t.fallback})
}

// broadcast queues one op on every sink. The caller holds r.mu, which keeps the
// order of ops the same in every editor and keeps a replay in step with them.
func (r *Reporter) broadcast(o op) {
	for s := range r.sinks {
		s.enqueue(o)
	}
}

// Attach sends the active conditions and the work in progress to client, then
// sends every later report to it until detach is called. progress tells
// whether the client supports work-done progress (the window.workDoneProgress
// client capability). Call Attach only after the client sent `initialized`.
func (r *Reporter) Attach(client protocol.Client, progress bool) (detach func()) {
	if r == nil || client == nil {
		return func() {}
	}
	s := &sink{
		client:   client,
		progress: progress,
		wake:     make(chan struct{}, 1),
		stop:     make(chan struct{}),
		created:  make(map[string]bool),
	}
	r.mu.Lock()
	entries := make([]*entry, 0, len(r.conds))
	for _, e := range r.conds {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].seq < entries[j].seq })
	for _, e := range entries {
		s.enqueue(op{kind: opShow, sev: e.Severity, message: e.Message})
	}
	tasks := make([]*Task, 0, len(r.tasks))
	for _, t := range r.tasks {
		tasks = append(tasks, t)
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].seq < tasks[j].seq })
	for _, t := range tasks {
		s.enqueue(t.beginOp())
	}
	r.sinks[s] = struct{}{}
	r.mu.Unlock()

	go s.run()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.sinks, s)
			r.mu.Unlock()
			close(s.stop)
		})
	}
}

type opKind uint8

const (
	opShow opKind = iota
	opBegin
	opReport
	opEnd
)

type op struct {
	kind     opKind
	sev      Severity
	message  string
	token    string
	title    string
	percent  int
	fallback bool
}

// sink delivers reports to one editor in order.
type sink struct {
	client   protocol.Client
	progress bool

	mu    sync.Mutex
	queue []op
	wake  chan struct{}
	stop  chan struct{}

	created map[string]bool // tokens the editor accepted; only run uses it
}

func (s *sink) enqueue(o op) {
	s.mu.Lock()
	if len(s.queue) >= maxQueue && o.kind == opReport {
		s.mu.Unlock()
		return
	}
	s.queue = append(s.queue, o)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *sink) run() {
	for {
		select {
		case <-s.stop:
			return
		case <-s.wake:
		}
		for {
			s.mu.Lock()
			if len(s.queue) == 0 {
				s.mu.Unlock()
				break
			}
			batch := s.queue
			s.queue = nil
			s.mu.Unlock()
			for _, o := range batch {
				select {
				case <-s.stop:
					return
				default:
				}
				s.deliver(o)
			}
		}
	}
}

func (s *sink) deliver(o op) {
	ctx, cancel := context.WithTimeout(context.Background(), deliveryTimeout)
	defer cancel()
	switch o.kind {
	case opShow:
		Show(ctx, s.client, o.sev, o.message)
	case opBegin:
		if s.progress {
			token := protocol.NewProgressToken(o.token)
			if err := s.client.WorkDoneProgressCreate(ctx, &protocol.WorkDoneProgressCreateParams{Token: *token}); err == nil {
				s.created[o.token] = true
				begin := protocol.WorkDoneProgressBegin{Kind: protocol.WorkDoneProgressKindBegin, Title: o.title, Message: o.message}
				if o.percent >= 0 {
					begin.Percentage = uint32(o.percent)
				}
				_ = s.client.Progress(ctx, &protocol.ProgressParams{Token: *token, Value: begin})
				return
			}
		}
		if o.fallback {
			text := o.title
			if o.message != "" {
				text += ": " + o.message
			}
			Show(ctx, s.client, Info, text)
		}
	case opReport:
		if !s.created[o.token] {
			return
		}
		report := protocol.WorkDoneProgressReport{Kind: protocol.WorkDoneProgressKindReport, Message: o.message}
		if o.percent >= 0 {
			report.Percentage = uint32(o.percent)
		}
		_ = s.client.Progress(ctx, &protocol.ProgressParams{Token: *protocol.NewProgressToken(o.token), Value: report})
	case opEnd:
		if s.created[o.token] {
			delete(s.created, o.token)
			_ = s.client.Progress(ctx, &protocol.ProgressParams{
				Token: *protocol.NewProgressToken(o.token),
				Value: protocol.WorkDoneProgressEnd{Kind: protocol.WorkDoneProgressKindEnd, Message: o.message},
			})
			return
		}
		if o.fallback && o.message != "" {
			Show(ctx, s.client, Info, o.message)
		}
	}
}

// Show sends one window/showMessage to one client. Use it only for a report
// that concerns that one editor, for example the result of its own request;
// workspace states go through a Reporter.
func Show(ctx context.Context, client protocol.Client, sev Severity, message string) {
	if client == nil {
		return
	}
	if err := client.ShowMessage(ctx, &protocol.ShowMessageParams{Type: sev.messageType(), Message: message}); err != nil {
		log.Printf("ShowMessage: %v", err)
	}
}

// Summarize writes a list of paths the way a message to the user needs it:
// the first path and how many more there are.
func Summarize(paths []string) string {
	switch len(paths) {
	case 0:
		return ""
	case 1:
		return paths[0]
	default:
		return fmt.Sprintf("%s and %d more", paths[0], len(paths)-1)
	}
}
