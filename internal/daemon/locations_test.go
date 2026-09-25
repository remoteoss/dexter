package daemon

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/remoteoss/dexter/internal/lsp"
	"github.com/remoteoss/dexter/internal/workspace"
)

// Every field survives the file-table encoding, order is kept even when paths
// interleave, and each path is sent once.
func TestLocationsRoundTripThroughFileTable(t *testing.T) {
	locations := []lsp.NameLocation{
		{FilePath: "/w/lib/b.ex", Line: 9, Kind: "call"},
		{FilePath: "/w/lib/a.ex", Line: 1, Kind: "function", Arity: 2, IsDeclaration: true},
		{FilePath: "/w/lib/b.ex", Line: 3, Kind: "alias"},
		{FilePath: "/w/lib/a.ex", Line: 7},
	}
	for _, result := range []any{
		&LookupResult{Locations: locations, Ready: true},
		&ReferencesResult{Locations: locations, Ready: true},
	} {
		encoded, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Count(string(encoded), "/w/lib/b.ex"); got != 1 {
			t.Fatalf("%T sent a repeated path %d times: %s", result, got, encoded)
		}
		decoded := reflect.New(reflect.TypeOf(result).Elem()).Interface()
		if err := json.Unmarshal(encoded, decoded); err != nil {
			t.Fatalf("%T: %v", result, err)
		}
		if !reflect.DeepEqual(decoded, result) {
			t.Fatalf("%T round trip = %+v, want %+v", result, decoded, result)
		}
	}
}

func TestEmptyLocationsEncodeAsEmptyLists(t *testing.T) {
	encoded, err := json.Marshal(ReferencesResult{Ready: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(encoded), `{"files":[],"locations":[],"ready":true}`; got != want {
		t.Fatalf("encoded = %s, want %s", got, want)
	}
	var decoded ReferencesResult
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Locations) != 0 || !decoded.Ready {
		t.Fatalf("decoded = %+v", decoded)
	}
}

// A location that names a file outside the table is a broken peer; decoding
// must fail rather than invent a path or panic.
func TestLocationsRejectFileIndexOutsideTable(t *testing.T) {
	for _, body := range []string{
		`{"files":["/w/a.ex"],"locations":[{"file":1,"line":1}],"ready":true}`,
		`{"files":["/w/a.ex"],"locations":[{"file":-1,"line":1}],"ready":true}`,
		`{"files":[],"locations":[{"file":0,"line":1}],"ready":true}`,
	} {
		var decoded LookupResult
		if err := json.Unmarshal([]byte(body), &decoded); err == nil {
			t.Fatalf("accepted %s as %+v", body, decoded)
		}
	}
}

// A change too large for one line still reaches the subscriber, as a full
// change, instead of silently ending the subscription.
func TestOversizedChangeIsReportedAsFull(t *testing.T) {
	var sent []Changed
	notify := func(method string, params any) error {
		change := params.(Changed)
		if len(change.Paths) > 0 {
			return errLineTooLarge
		}
		sent = append(sent, change)
		return nil
	}
	if err := notifyChange(notify, "sub-1", workspace.Change{Paths: []string{"/w/a.ex"}}); err != nil {
		t.Fatal(err)
	}
	if want := []Changed{{Subscription: "sub-1", Full: true}}; !reflect.DeepEqual(sent, want) {
		t.Fatalf("sent %+v, want %+v", sent, want)
	}

	failed := errors.New("connection closed")
	err := notifyChange(func(string, any) error { return failed }, "sub-1", workspace.Change{Paths: []string{"/w/a.ex"}})
	if !errors.Is(err, failed) {
		t.Fatalf("error = %v, want the write failure", err)
	}
}
