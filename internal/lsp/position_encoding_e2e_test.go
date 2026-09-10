package lsp

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"

	"go.lsp.dev/protocol"
)

// utf16Col is the column a UTF-16 client reports for the first occurrence of
// needle in line — what Neovim, VS Code and the rest actually put on the wire.
func utf16Col(line, needle string) uint32 {
	i := strings.Index(line, needle)
	if i < 0 {
		return 0
	}
	return uint32(len(utf16.Encode([]rune(line[:i]))))
}

// byteCol is the same position counted in UTF-8 bytes, which is what Dexter
// works in internally.
func byteCol(line, needle string) uint32 {
	i := strings.Index(line, needle)
	if i < 0 {
		return 0
	}
	return uint32(i)
}

// The caller line carries three 2-byte characters before the call, so a
// UTF-16 client and a byte-counting server disagree by exactly three.
const nonASCIISource = `defmodule MyApp.Prices do
  alias MyApp.Money

  def table do
    %{"São Tomé and Príncipe" => Money.new(:EUR, "19.95")}
  end
end`

const moneySource = `defmodule MyApp.Money do
  def new(currency, amount), do: {currency, amount}
end`

func setupNonASCIIServer(t *testing.T, enc PositionEncoding) (*Server, string, string, func()) {
	t.Helper()
	server, cleanup := setupTestServer(t)
	server.positionEncoding = enc

	indexFile(t, server.store, server.projectRoot, "lib/money.ex", moneySource)
	indexFile(t, server.store, server.projectRoot, "lib/prices.ex", nonASCIISource)

	fileURI := "file://" + filepath.Join(server.projectRoot, "lib/prices.ex")
	server.docs.Set(fileURI, nonASCIISource)

	// Line 4 (0-based) is the one holding the accented characters.
	line := strings.Split(nonASCIISource, "\n")[4]
	return server, fileURI, line, cleanup
}

// TestDefinition_UTF16ClientOnNonASCIILine is the original bug: a client
// counting UTF-16 units asks about a line with accented characters on it.
// The server used to read that column as a byte offset, land three bytes
// short of the call, and find nothing.
func TestDefinition_UTF16ClientOnNonASCIILine(t *testing.T) {
	server, fileURI, line, cleanup := setupNonASCIIServer(t, EncodingUTF16)
	defer cleanup()

	col := utf16Col(line, "Money")
	if col == byteCol(line, "Money") {
		t.Fatalf("test line must make the two encodings disagree; both are %d", col)
	}

	locs := definitionAt(t, server, fileURI, 4, col)
	if len(locs) == 0 {
		t.Fatalf("no definition at utf-16 column %d on %q", col, line)
	}
	if got := filepath.Base(uriToPath(locs[0].URI)); got != "money.ex" {
		t.Errorf("resolved to %s, want money.ex", got)
	}
}

// TestDefinition_UTF8ClientOnNonASCIILine covers the utf-8 branch, which
// nothing selects today because Dexter does not negotiate. It is kept so the
// branch stays honest if that changes: with utf-8 set, columns are byte
// offsets and pass through untouched.
func TestDefinition_UTF8ClientOnNonASCIILine(t *testing.T) {
	server, fileURI, line, cleanup := setupNonASCIIServer(t, EncodingUTF8)
	defer cleanup()

	locs := definitionAt(t, server, fileURI, 4, byteCol(line, "Money"))
	if len(locs) == 0 {
		t.Fatalf("no definition at byte column %d on %q", byteCol(line, "Money"), line)
	}
	if got := filepath.Base(uriToPath(locs[0].URI)); got != "money.ex" {
		t.Errorf("resolved to %s, want money.ex", got)
	}
}

// TestRenameEdit_UTF16RangesLandOnTheRightText is the case that damaged
// files. The server used to answer with byte columns whatever the client
// counted in; an editor applying those as UTF-16 replaced a span shifted off
// the identifier, quietly corrupting the line.
//
// Applying the reply exactly as a UTF-16 client would is the only way to
// catch that, so that is what this asserts.
func TestRenameEdit_UTF16RangesLandOnTheRightText(t *testing.T) {
	server, fileURI, line, cleanup := setupNonASCIIServer(t, EncodingUTF16)
	defer cleanup()

	edit, err := server.RenameEdit(context.Background(), &protocol.RenameParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(fileURI)},
			Position:     protocol.Position{Line: 4, Character: utf16Col(line, "Money")},
		},
		NewName: "Cash",
	})
	if err != nil {
		t.Fatal(err)
	}
	if edit == nil {
		t.Fatal("rename produced no edit")
	}

	edits := editsForURI(edit, fileURI)
	if len(edits) == 0 {
		t.Fatalf("no edits for %s", fileURI)
	}

	for _, e := range edits {
		if e.Range.Start.Line != 4 {
			continue
		}
		got := sliceUTF16(line, e.Range.Start.Character, e.Range.End.Character)
		if got != "Money" {
			t.Errorf("edit range [%d,%d) selects %q as a utf-16 client reads it, want %q\n  line: %s",
				e.Range.Start.Character, e.Range.End.Character, got, "Money", line)
		}
	}
}

// sliceUTF16 extracts [start,end) counted in UTF-16 units, which is how the
// editor will read the range the server sent.
func sliceUTF16(line string, start, end uint32) string {
	units := utf16.Encode([]rune(line))
	if int(start) > len(units) {
		start = uint32(len(units))
	}
	if int(end) > len(units) {
		end = uint32(len(units))
	}
	return string(utf16.Decode(units[start:end]))
}

func editsForURI(edit *WorkspaceEdit, fileURI string) []protocol.TextEdit {
	if e, ok := edit.Changes[protocol.DocumentURI(fileURI)]; ok {
		return e
	}
	for _, dc := range edit.DocumentChanges {
		tde, ok := dc.(TextDocumentEdit)
		if ok && string(tde.TextDocument.URI) == fileURI {
			return tde.Edits
		}
	}
	return nil
}
