package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.lsp.dev/protocol"
)

type CallHierarchyParams struct {
	Module    string `json:"module" jsonschema:"fully-qualified module owning the function"`
	Function  string `json:"function" jsonschema:"function name without arity"`
	Direction string `json:"direction,omitempty" jsonschema:"'incoming' (callers), 'outgoing' (callees), or 'both' (default)"`
}

const maxCallsPerDirection = 50

func (h *Handler) callHierarchyHandler(ctx context.Context, req *mcp.CallToolRequest, args CallHierarchyParams) (*mcp.CallToolResult, any, error) {
	module := strings.TrimSpace(args.Module)
	function := strings.TrimSpace(args.Function)
	if module == "" || function == "" {
		return nil, nil, fmt.Errorf("module and function must not be empty")
	}
	direction := strings.ToLower(strings.TrimSpace(args.Direction))
	switch direction {
	case "":
		direction = "both"
	case "incoming", "outgoing", "both":
	default:
		return nil, nil, fmt.Errorf("direction must be 'incoming', 'outgoing', or 'both', got %q", args.Direction)
	}

	// The LSP call-hierarchy handlers are name-based: they only read the
	// module/function pair from Item.Data, so a synthetic item works.
	item := protocol.CallHierarchyItem{
		Data: map[string]interface{}{"module": module, "function": function},
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Call hierarchy for %s.%s:\n", module, function)
	found := false

	if direction == "incoming" || direction == "both" {
		calls, err := h.lsp.IncomingCalls(ctx, &protocol.CallHierarchyIncomingCallsParams{Item: item})
		if err != nil {
			return nil, nil, fmt.Errorf("incoming calls: %w", err)
		}
		fmt.Fprintf(&b, "\nIncoming (callers): %d\n", len(calls))
		for i, c := range calls {
			if i == maxCallsPerDirection {
				fmt.Fprintf(&b, "  ... and %d more\n", len(calls)-maxCallsPerDirection)
				break
			}
			lines := make([]string, 0, len(c.FromRanges))
			for _, r := range c.FromRanges {
				lines = append(lines, fmt.Sprintf("%d", r.Start.Line+1))
			}
			fmt.Fprintf(&b, "  ← %s (%s:%d) calls at line %s\n", c.From.Name, h.relPath(uriToPath(c.From.URI)), c.From.Range.Start.Line+1, strings.Join(lines, ", "))
		}
		found = found || len(calls) > 0
	}

	if direction == "outgoing" || direction == "both" {
		calls, err := h.lsp.OutgoingCalls(ctx, &protocol.CallHierarchyOutgoingCallsParams{Item: item})
		if err != nil {
			return nil, nil, fmt.Errorf("outgoing calls: %w", err)
		}
		fmt.Fprintf(&b, "\nOutgoing (callees): %d\n", len(calls))
		for i, c := range calls {
			if i == maxCallsPerDirection {
				fmt.Fprintf(&b, "  ... and %d more\n", len(calls)-maxCallsPerDirection)
				break
			}
			fmt.Fprintf(&b, "  → %s (%s:%d)\n", c.To.Name, h.relPath(uriToPath(c.To.URI)), c.To.Range.Start.Line+1)
		}
		found = found || len(calls) > 0
	}

	if !found {
		fmt.Fprintf(&b, "\nNo calls found. Check the module/function names (dexter_search can help), or call dexter_reindex if files changed recently.\n")
	}
	return textResult(b.String()), nil, nil
}

func uriToPath(u protocol.DocumentURI) string {
	return u.Filename()
}
