package daemon

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"

	"go.lsp.dev/uri"

	"github.com/remoteoss/dexter/internal/lsp"
	"github.com/remoteoss/dexter/internal/store"
)

// Routing: each file goes to the daemon of its own project. The project of a
// file is the nearest nested clone or linked worktree between it and the
// start root, else the start root (see projectRoot). A file outside the
// start root (a dependency or a
// standard library file that a definition opened) has no project to start a
// daemon for, so it goes to the primary, or for a container to the upstream
// that served the last file. Directories are looked up once per session.

// maxRoutedDirs bounds the directory cache; past it, the cache starts again.
const maxRoutedDirs = 4096

// upstreamFor returns the upstream for a message about the URI whose JSON
// string is rawURI, and starts it when its project has none yet. It returns
// nil when no upstream can take the message (a container whose session has
// no project yet). The editor sends many messages about one file in a row,
// so the last answer is kept, and a repeat costs one comparison.
func (p *lspProxy) upstreamFor(rawURI []byte, method string) *upstream {
	var u *upstream
	fallback := false
	// A cached fallback whose upstream failed is looked up again, as route
	// does.
	if p.lastUp != nil && string(rawURI) == p.lastURI && (!p.lastFall || !p.lastUp.isFailed()) {
		u, fallback = p.lastUp, p.lastFall
	} else {
		u, fallback = p.route(unquote(rawURI), true)
		p.lastURI, p.lastUp, p.lastFall = string(rawURI), u, fallback
	}
	if !fallback {
		if p.container && u != nil {
			p.fallbackUp = u
		}
		return u
	}
	// A file outside every project of a container: its document stays
	// where its didOpen went.
	if p.primary == nil {
		docURI := unquote(rawURI)
		if doc := p.fallbackDocs[docURI]; doc != nil {
			u = doc
		}
		switch method {
		case "textDocument/didOpen":
			if u != nil {
				p.fallbackDocs[docURI] = u
			}
		case "textDocument/didClose":
			delete(p.fallbackDocs, docURI)
		}
	}
	return u
}

// route finds the upstream of a file URI. fallback reports a file that has
// no project of its own below the start root; then the result is the
// primary or, for a container, the upstream of the last such file.
func (p *lspProxy) route(docURI string, create bool) (u *upstream, fallback bool) {
	root := ""
	if path := lsp.URIPath(docURI); path != "" {
		dir := filepath.Dir(path)
		var ok bool
		if root, ok = p.roots[dir]; !ok {
			if len(p.roots) >= maxRoutedDirs {
				p.roots = make(map[string]string)
			}
			root = p.projectRoot(dir)
			p.roots[dir] = root
		}
	}
	if root != "" {
		key, ok := p.rootKeys[root]
		if !ok {
			key = resolveRootKey(root)
			p.rootKeys[root] = key
		}
		// Another spelling of the start root is the start root.
		if key != p.rootKey {
			p.mu.Lock()
			u = p.upstreams[key]
			p.mu.Unlock()
			if u == nil && create {
				u = p.addUpstream(root, key)
			}
			return u, false
		}
	}
	if p.primary != nil {
		return p.primary, false
	}
	if p.fallbackUp != nil && p.fallbackUp.isFailed() {
		p.fallbackUp = nil
	}
	return p.fallbackUp, true
}

// projectRoot returns the root of the project that owns the files of dir, or
// "" when that is the start root, or dir is not below the start root.
//
// Below the start root, a file belongs to another project only when a nested
// clone (a .git directory) or a linked worktree is between the file and the
// start root; the nearest one is the project. An index alone does not make a
// project: a Dexter index left in apps/foo of a monorepo is not one. A
// submodule (a .git file that is not a linked worktree) stays a part of the
// project that holds it. The rule is the same for a container.
func (p *lspProxy) projectRoot(dir string) string {
	base := p.root
	if !within(dir, base) {
		if p.rootKey == base || !within(dir, p.rootKey) {
			return ""
		}
		base = p.rootKey // the start root through its resolved spelling
	}
	return store.NestedProject(dir, base)
}

// within reports whether path is base or below it.
func within(path, base string) bool {
	if path == base {
		return true
	}
	if !strings.HasPrefix(path, base) {
		return false
	}
	return strings.HasSuffix(base, string(filepath.Separator)) || path[len(base)] == filepath.Separator
}

// resolveRootKey is the identity that the daemon of root has (see
// workspaceIdentity): two spellings of one project, a symlink or another
// letter case on a case-insensitive file system, share one upstream, as they
// share one daemon.
func resolveRootKey(root string) string {
	return workspaceIdentity(root)
}

func (u *upstream) isFailed() bool {
	u.connMu.Lock()
	defer u.connMu.Unlock()
	return u.failed
}

// upstreamInitialize is the editor's initialize request for the daemon of
// another project: the same capabilities and options, with the roots of that
// project and the proxy's id.
func upstreamInitialize(body []byte, root, id string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	params := map[string]json.RawMessage{}
	if raw := bytes.TrimSpace(fields["params"]); len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
	}
	rootURI := string(uri.File(root))
	encode := func(v any) json.RawMessage {
		raw, _ := json.Marshal(v)
		return raw
	}
	params["rootUri"] = encode(rootURI)
	params["rootPath"] = encode(root)
	params["workspaceFolders"] = encode([]map[string]string{{"uri": rootURI, "name": filepath.Base(root)}})
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	fields["params"] = raw
	fields["id"] = json.RawMessage(id)
	return json.Marshal(fields)
}

// upstreamToken is the token that the editor gets for a progress token of a
// secondary daemon: a string with the upstream's number, then the daemon's
// token as JSON, so that the proxy can give it back.
func upstreamToken(n int, token string) string {
	return strconv.Quote("dexter-u" + strconv.Itoa(n) + ":" + token)
}

// splitUpstreamToken undoes upstreamToken.
func splitUpstreamToken(token string) (n int, daemonToken string, ok bool) {
	s, err := strconv.Unquote(token)
	if err != nil || !strings.HasPrefix(s, "dexter-u") {
		return 0, "", false
	}
	num, rest, found := strings.Cut(s[len("dexter-u"):], ":")
	if !found {
		return 0, "", false
	}
	n, err = strconv.Atoi(num)
	if err != nil || n <= 0 || rest == "" {
		return 0, "", false
	}
	return n, rest, true
}

// cancelProgress passes window/workDoneProgress/cancel to the daemon that
// made the token.
func (p *lspProxy) cancelProgress(frame []byte, body int, msg peeked) {
	target := p.primary
	var fields, params map[string]json.RawMessage
	if json.Unmarshal(frame[body:], &fields) == nil && json.Unmarshal(fields["params"], &params) == nil {
		if n, daemonToken, ok := splitUpstreamToken(string(bytes.TrimSpace(params["token"]))); ok {
			target = nil
			for _, u := range p.allUpstreams() {
				if u.n == n {
					target = u
				}
			}
			params["token"] = json.RawMessage(daemonToken)
			if raw, err := json.Marshal(params); err == nil {
				fields["params"] = raw
				if out, err := json.Marshal(fields); err == nil {
					frame = frameMessage(out)
				}
			}
		}
	}
	if target == nil {
		return
	}
	target.connMu.Lock()
	target.forwardLocked(frame, msg, false)
	target.connMu.Unlock()
}

// splitWatchedFiles sends each change of workspace/didChangeWatchedFiles to
// the upstream of its file. It starts no upstream: the daemon of a project
// that no editor file uses watches its files itself.
func (p *lspProxy) splitWatchedFiles(frame []byte, body int, msg peeked) {
	ups := p.allUpstreams()
	if len(ups) == 0 {
		return
	}
	if len(ups) == 1 && ups[0] == p.primary {
		u := p.primary
		u.connMu.Lock()
		u.forwardLocked(frame, msg, false)
		u.connMu.Unlock()
		return
	}
	var params struct {
		Params struct {
			Changes []json.RawMessage `json:"changes"`
		} `json:"params"`
	}
	if json.Unmarshal(frame[body:], &params) != nil {
		return
	}
	groups := make(map[*upstream][]json.RawMessage)
	var order []*upstream
	for _, change := range params.Params.Changes {
		var c struct {
			URI string `json:"uri"`
		}
		if json.Unmarshal(change, &c) != nil {
			continue
		}
		u, fallback := p.route(c.URI, false)
		if u == nil || (fallback && p.primary == nil) {
			continue
		}
		if groups[u] == nil {
			order = append(order, u)
		}
		groups[u] = append(groups[u], change)
	}
	for _, u := range order {
		out, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"method":  "workspace/didChangeWatchedFiles",
			"params":  map[string]any{"changes": groups[u]},
		})
		if err != nil {
			continue
		}
		u.connMu.Lock()
		u.forwardLocked(frameMessage(out), msg, false)
		u.connMu.Unlock()
	}
}

// fanout is an editor request that went to several upstreams: shutdown, and
// workspace/symbol, whose results are joined.
type fanout struct {
	editorID  []byte
	method    string
	remaining int
	results   [][]byte // the array results of the parts
	firstErr  []byte
	answered  bool
	parts     map[string]*upstream // part id -> upstream, while it is open
}

// fanOut sends a request to every upstream and answers the editor once all
// answered. With one upstream, the request goes to it as it is.
func (p *lspProxy) fanOut(frame []byte, body int, msg peeked) {
	shutdown := msg.method == "shutdown"
	var targets []*upstream
	for _, u := range p.allUpstreams() {
		u.connMu.Lock()
		ok := !u.failed && (shutdown || u.conn != nil)
		u.connMu.Unlock()
		if ok {
			targets = append(targets, u)
		}
	}
	if len(targets) == 0 && !shutdown && p.primary != nil && !p.primary.isFailed() {
		// The primary has no daemon now: the request waits for the next.
		targets = []*upstream{p.primary}
	}
	switch len(targets) {
	case 0:
		if shutdown {
			p.recordSession(frame, body, msg)
		}
		if !msg.isRequest() {
			return
		}
		if shutdown {
			p.answerNull(msg.id)
		} else {
			p.writeOut(frameMessage([]byte(`{"jsonrpc":"2.0","id":` + string(msg.id) + `,"result":[]}`)))
		}
		return
	case 1:
		u := targets[0]
		u.connMu.Lock()
		if shutdown {
			p.recordSession(frame, body, msg)
		}
		u.forwardLocked(frame, msg, false)
		u.connMu.Unlock()
		return
	}
	if !msg.isRequest() {
		p.broadcast(frame, body, msg, shutdown)
		return
	}
	for _, u := range targets {
		u.connMu.Lock()
	}
	defer func() {
		for i := len(targets) - 1; i >= 0; i-- {
			targets[i].connMu.Unlock()
		}
	}()
	if shutdown {
		p.recordSession(frame, body, msg)
	}
	f := &fanout{editorID: append([]byte(nil), msg.id...), method: msg.method, parts: make(map[string]*upstream)}
	type part struct {
		u     *upstream
		id    string
		frame []byte
	}
	parts := make([]part, 0, len(targets))
	p.mu.Lock()
	p.fanSeq++
	seq := p.fanSeq
	p.mu.Unlock()
	for i, u := range targets {
		id := quoteID("dexter-proxy-fan-%d-%d", seq, i)
		out, err := withID(frame[body:], id)
		if err != nil {
			continue
		}
		parts = append(parts, part{u, id, frameMessage(out)})
		f.parts[id] = u
	}
	if len(parts) == 0 {
		p.answerError(msg.id, lspInternalError, "Dexter: the request cannot be read.")
		return
	}
	f.remaining = len(parts)
	p.mu.Lock()
	for _, pt := range parts {
		p.fanouts[pt.id] = f
	}
	p.fanByEditor[string(msg.id)] = f
	p.mu.Unlock()
	for _, pt := range parts {
		pt.u.forwardLocked(pt.frame, peeked{method: msg.method, id: []byte(pt.id)}, false)
	}
}

// completePart takes the answer to one part of a fanout. body is the
// daemon's answer, or nil for a part that no daemon will answer. It reports
// false when id is not a part. When the last part is in, the editor gets
// the answer.
func (p *lspProxy) completePart(id, body []byte) bool {
	key := string(id)
	p.mu.Lock()
	f := p.fanouts[key]
	if f == nil {
		p.mu.Unlock()
		return false
	}
	delete(p.fanouts, key)
	delete(f.parts, key)
	f.remaining--
	if body != nil {
		var answer struct {
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(body, &answer) == nil {
			if r := bytes.TrimSpace(answer.Result); len(r) > 1 && r[0] == '[' {
				f.results = append(f.results, r)
			} else if e := bytes.TrimSpace(answer.Error); len(e) > 0 && string(e) != "null" && f.firstErr == nil {
				f.firstErr = e
			}
		}
	}
	done := f.remaining == 0 && !f.answered
	if f.remaining == 0 {
		f.answered = true
		delete(p.fanByEditor, string(f.editorID))
	}
	p.mu.Unlock()
	if done {
		p.writeOut(f.answer())
	}
	return true
}

// answer is the editor's answer to a fanout whose parts are all in.
func (f *fanout) answer() []byte {
	if f.method == "shutdown" {
		return nullResult(f.editorID)
	}
	if len(f.results) == 0 && f.firstErr != nil {
		return frameMessage([]byte(`{"jsonrpc":"2.0","id":` + string(f.editorID) + `,"error":` + string(f.firstErr) + `}`))
	}
	var b bytes.Buffer
	b.WriteString(`{"jsonrpc":"2.0","id":`)
	b.Write(f.editorID)
	b.WriteString(`,"result":[`)
	first := true
	for _, r := range f.results {
		inner := bytes.TrimSpace(r[1 : len(r)-1])
		if len(inner) == 0 {
			continue
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		b.Write(inner)
	}
	b.WriteString(`]}`)
	return frameMessage(b.Bytes())
}

// cancelFanout cancels a fanout that the editor canceled: the editor gets
// RequestCancelled at once, and each daemon a cancel for its part. It
// reports false when id is not a fanout.
func (p *lspProxy) cancelFanout(id []byte) bool {
	p.mu.Lock()
	f := p.fanByEditor[string(id)]
	if f == nil || f.answered {
		p.mu.Unlock()
		return false
	}
	f.answered = true
	delete(p.fanByEditor, string(id))
	parts := make(map[string]*upstream, len(f.parts))
	for partID, u := range f.parts {
		parts[partID] = u
	}
	p.mu.Unlock()
	p.answerError(id, lspRequestCancelled, "Dexter: the request was canceled.")
	for partID, u := range parts {
		u.connMu.Lock()
		u.cancelLocked(cancelFrame(partID), []byte(partID))
		u.connMu.Unlock()
	}
	return true
}
