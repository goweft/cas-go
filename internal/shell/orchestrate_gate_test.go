package shell

import (
	"testing"

	"github.com/goweft/cas/internal/workspace"
)

// Gate table for orchestration. The single-workspace tool-bearing cases
// are the regression: right after a user's first ingest, the advertised
// "use this server to <task>" used to fall through silently to the
// tool-less ChatAgent because the gate required two workspaces.
func TestOrchestratable(t *testing.T) {
	ws := func(types ...string) []*workspace.Workspace {
		out := make([]*workspace.Workspace, len(types))
		for i, ty := range types {
			out[i] = &workspace.Workspace{Type: ty}
		}
		return out
	}

	cases := []struct {
		name   string
		active []*workspace.Workspace
		want   bool
	}{
		{"zero workspaces", ws(), false},
		{"one mcp workspace", ws("mcp"), true},
		{"one web workspace", ws("web"), true},
		{"one document workspace", ws("document"), false},
		{"one code workspace", ws("code"), false},
		{"two documents", ws("document", "document"), true},
		{"mcp plus document", ws("mcp", "document"), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := orchestratable(tc.active); got != tc.want {
				t.Errorf("orchestratable(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}
