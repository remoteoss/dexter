package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"sync"

	"github.com/remoteoss/dexter/internal/lsp"
	"github.com/remoteoss/dexter/internal/workspace"
)

// This file is the daemon's extension surface. A new protocol adapter or a new
// control method registers here instead of editing the daemon, so frontends stay
// independent and merges do not collide in daemon internals.

// FrontendConn is one accepted adapter connection after the handshake.
type FrontendConn struct {
	// Conn is the raw socket. Use Reader instead of reading Conn directly: it
	// may already hold bytes read alongside the handshake response.
	Conn   net.Conn
	Reader *bufio.Reader
	// Context is canceled when the connection ends or the daemon stops.
	Context context.Context
	// Runtime is the workspace this daemon owns.
	Runtime *workspace.Runtime
	// Session is the editor session this connection explicitly attached to,
	// or "" when it did not name one.
	Session string
	// LSP resolves the language service for this connection: the named editor
	// session when one was requested, otherwise the headless instance.
	LSP func() *lsp.Server
	// Notify pushes one server-initiated message to this client. It is safe
	// for concurrent use and serializes writes on the socket.
	Notify func(method string, params any) error
	// Done is closed when the connection ends. Long-lived subscriptions must
	// stop on it.
	Done <-chan struct{}
}

// Frontend serves one connection of a registered kind. It owns the connection
// until Serve returns.
type Frontend interface {
	Serve(fc FrontendConn) error
}

// MethodContext is what a registered control method may use.
type MethodContext struct {
	Context context.Context
	Runtime *workspace.Runtime
	Session string
	LSP     func() *lsp.Server
	Notify  func(method string, params any) error
	Done    <-chan struct{}
}

// MethodHandler implements one control method. Returning a nil result sends a
// response with no payload.
type MethodHandler func(mc MethodContext, params json.RawMessage) (any, error)

var (
	frontendMu sync.RWMutex
	frontends  = make(map[string]Frontend)

	methodMu sync.RWMutex
	methods  = make(map[string]MethodHandler)
)

// RegisterFrontend installs the adapter served for a handshake kind. Kinds
// "control" and "lsp" are reserved. Registration happens during package
// initialization, so a duplicate kind is a programming error and panics.
func RegisterFrontend(kind string, f Frontend) {
	if kind == "" || kind == kindControl || kind == kindLSP {
		panic(fmt.Sprintf("daemon: reserved frontend kind %q", kind))
	}
	frontendMu.Lock()
	defer frontendMu.Unlock()
	if _, exists := frontends[kind]; exists {
		panic(fmt.Sprintf("daemon: frontend kind %q already registered", kind))
	}
	frontends[kind] = f
}

func lookupFrontend(kind string) (Frontend, bool) {
	frontendMu.RLock()
	defer frontendMu.RUnlock()
	f, ok := frontends[kind]
	return f, ok
}

// RegisteredFrontends lists the handshake kinds this binary can serve, for
// diagnostics.
func RegisteredFrontends() []string {
	frontendMu.RLock()
	defer frontendMu.RUnlock()
	out := make([]string, 0, len(frontends))
	for kind := range frontends {
		out = append(out, kind)
	}
	sort.Strings(out)
	return out
}

// RegisterMethod adds a control method. The daemon's built-in methods are
// reserved: registering one panics, because a frontend silently replacing
// workspace ownership semantics would be a correctness bug, not an extension.
func RegisterMethod(name string, h MethodHandler) {
	if isBuiltinMethod(name) {
		panic(fmt.Sprintf("daemon: %q is a built-in control method", name))
	}
	methodMu.Lock()
	defer methodMu.Unlock()
	if _, exists := methods[name]; exists {
		panic(fmt.Sprintf("daemon: control method %q already registered", name))
	}
	methods[name] = h
}

func lookupMethod(name string) (MethodHandler, bool) {
	methodMu.RLock()
	defer methodMu.RUnlock()
	h, ok := methods[name]
	return h, ok
}

func isBuiltinMethod(name string) bool {
	switch name {
	case MethodStatus, MethodShutdown, MethodWorkspaceStatus, MethodLookup,
		MethodReferences, MethodReindex, MethodImpactSnapshot, MethodWatch, MethodUnwatch:
		return true
	}
	return false
}
