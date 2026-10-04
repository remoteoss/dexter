package daemon

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
)

// Names in this file are unique per test because registration is process-global:
// there is no unregister, by design, so an adapter cannot be swapped under a
// running daemon.

func assertPanics(t *testing.T, what string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("%s did not panic", what)
		}
	}()
	fn()
}

func TestRegisteredMethodIsDispatched(t *testing.T) {
	RegisterMethod("registrytest/echo", func(mc MethodContext, params json.RawMessage) (any, error) {
		var payload struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(params, &payload); err != nil {
			return nil, err
		}
		if mc.Context == nil {
			t.Error("method context has no request context")
		}
		return map[string]string{"echo": payload.Value}, nil
	})

	handler, ok := lookupMethod("registrytest/echo")
	if !ok {
		t.Fatal("registered method was not found")
	}
	// Only the dispatch contract is checked here. That the daemon populates
	// Runtime, LSP, Session, Notify, and Done is asserted over a real socket in
	// server_test.go, where a registered method can answer from the workspace.
	result, err := handler(MethodContext{Context: context.Background()}, json.RawMessage(`{"value":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(encoded), `{"echo":"hi"}`; got != want {
		t.Fatalf("result = %s, want %s", got, want)
	}
}

// Built-ins own workspace semantics — shutdown, reindex barriers, the watch
// subscription. An adapter silently replacing one would be a correctness bug, so
// registration refuses rather than shadowing it.
func TestRegisterMethodRejectsBuiltins(t *testing.T) {
	for _, name := range []string{
		MethodStatus, MethodShutdown, MethodWorkspaceStatus, MethodLookup,
		MethodReferences, MethodReindex, MethodWatch, MethodUnwatch,
	} {
		assertPanics(t, "RegisterMethod("+name+")", func() {
			RegisterMethod(name, func(MethodContext, json.RawMessage) (any, error) { return nil, nil })
		})
	}
}

func TestRegisterMethodRejectsDuplicates(t *testing.T) {
	handler := func(MethodContext, json.RawMessage) (any, error) { return nil, nil }
	RegisterMethod("registrytest/duplicate", handler)
	assertPanics(t, "second RegisterMethod(registrytest/duplicate)", func() {
		RegisterMethod("registrytest/duplicate", handler)
	})
}

// "control" and "lsp" are the daemon's own dispatch paths; an adapter claiming
// either would bypass handshake validation or the LSP fast path.
func TestRegisterFrontendRejectsReservedKinds(t *testing.T) {
	frontend := stubFrontend{}
	for _, kind := range []string{"", kindControl, kindLSP} {
		assertPanics(t, "RegisterFrontend("+kind+")", func() { RegisterFrontend(kind, frontend) })
	}
}

func TestRegisterFrontendIsListedAndDispatchable(t *testing.T) {
	frontend := stubFrontend{}
	RegisterFrontend("registrytest-adapter", frontend)
	assertPanics(t, "second RegisterFrontend(registrytest-adapter)", func() {
		RegisterFrontend("registrytest-adapter", frontend)
	})

	got, ok := lookupFrontend("registrytest-adapter")
	if !ok || got != Frontend(frontend) {
		t.Fatalf("lookupFrontend = %v, %v", got, ok)
	}
	if !slices.Contains(RegisteredFrontends(), "registrytest-adapter") {
		t.Fatalf("registered frontend missing from %v", RegisteredFrontends())
	}
	if _, ok := lookupFrontend("registrytest-missing"); ok {
		t.Fatal("an unregistered kind resolved")
	}
}

type stubFrontend struct{}

func (stubFrontend) Serve(FrontendConn) error { return nil }
