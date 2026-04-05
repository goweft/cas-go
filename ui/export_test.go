package ui_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/goweft/cas/internal/shell"
	"github.com/goweft/cas/internal/store"
	"github.com/goweft/cas/internal/workspace"
	"github.com/goweft/cas/ui"
)

// ── Export helper tests ───────────────────────────────────────────

func TestExportExtDocument(t *testing.T) {
	ext := ui.ExportExt("document", "# My Document\n\nSome content")
	if ext != ".md" {
		t.Errorf("expected .md for document, got %q", ext)
	}
}

func TestExportExtList(t *testing.T) {
	ext := ui.ExportExt("list", "# Shopping List\n\n- Milk\n- Eggs")
	if ext != ".md" {
		t.Errorf("expected .md for list, got %q", ext)
	}
}

func TestExportExtCodePython(t *testing.T) {
	content := "import json\n\ndef parse_json(data):\n    return json.loads(data)\n\nif __name__ == '__main__':\n    print(parse_json('{}'))"
	ext := ui.ExportExt("code", content)
	if ext != ".py" {
		t.Errorf("expected .py for Python code, got %q", ext)
	}
}

func TestExportExtCodeGo(t *testing.T) {
	content := "package main\n\nimport \"fmt\"\n\nfunc main() {\n    fmt.Println(\"hello\")\n}"
	ext := ui.ExportExt("code", content)
	if ext != ".go" {
		t.Errorf("expected .go for Go code, got %q", ext)
	}
}

func TestExportExtCodeBash(t *testing.T) {
	content := "#!/bin/bash\necho hello world"
	ext := ui.ExportExt("code", content)
	if ext != ".sh" {
		t.Errorf("expected .sh for bash script, got %q", ext)
	}
}

func TestExportExtCodeUnknown(t *testing.T) {
	content := "some unknown code format with no language markers"
	ext := ui.ExportExt("code", content)
	if ext != ".txt" {
		t.Errorf("expected .txt for unknown code, got %q", ext)
	}
}

func TestExportWorkspaceCreatesFile(t *testing.T) {
	// Override home dir for test isolation
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	path, err := ui.ExportWorkspace("My Project Proposal", "document", "# My Project\n\nContent here")
	if err != nil {
		t.Fatalf("export failed: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("exported file does not exist: %v", err)
	}
	if !strings.HasSuffix(path, ".md") {
		t.Errorf("expected .md extension, got %q", path)
	}
}

func TestExportWorkspaceSanitizesTitle(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Title with characters invalid in filenames
	path, err := ui.ExportWorkspace("My/Project:Report*2024", "document", "content")
	if err != nil {
		t.Fatalf("export failed: %v", err)
	}
	filename := filepath.Base(path)
	for _, bad := range []string{"/", ":", "*"} {
		if strings.Contains(filename, bad) {
			t.Errorf("filename %q still contains bad character %q", filename, bad)
		}
	}
}

func TestExportWorkspaceWritesContent(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	content := "# Test\n\nHello export world"
	path, err := ui.ExportWorkspace("Test Export", "document", content)
	if err != nil {
		t.Fatalf("export failed: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read exported file: %v", err)
	}
	if string(got) != content {
		t.Errorf("exported content mismatch:\n  got:  %q\n  want: %q", string(got), content)
	}
}

func TestExportWorkspaceLongTitle(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	long := strings.Repeat("A", 100)
	path, err := ui.ExportWorkspace(long, "document", "content")
	if err != nil {
		t.Fatalf("export failed: %v", err)
	}
	filename := filepath.Base(path)
	// Without extension, should be truncated to 64 chars
	nameNoExt := strings.TrimSuffix(filename, filepath.Ext(filename))
	if len(nameNoExt) > 64 {
		t.Errorf("filename %q exceeds 64 chars (got %d)", nameNoExt, len(nameNoExt))
	}
}

func TestExportWorkspaceCreatesDir(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	path, err := ui.ExportWorkspace("Doc", "document", "content")
	if err != nil {
		t.Fatalf("export failed: %v", err)
	}
	dir := filepath.Dir(path)
	if !strings.HasSuffix(dir, "cas-exports") {
		t.Errorf("expected cas-exports dir, got %q", dir)
	}
}

// ── Ctrl+E keybinding test ────────────────────────────────────────

func TestCtrlEExportsInWorkspaceFocus(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	s := store.NewMemoryStore()
	sh := shell.NewShell(s)
	sess, _ := sh.CreateSession()

	// Seed with a real workspace
	ws, err := sh.Workspaces().Create("ws1", "document", "My Proposal", "# My Proposal\n\nContent", sess.ID)
	if err != nil {
		t.Fatal(err)
	}

	m := ui.New(sh, sess.ID, nil, []*workspace.Workspace{ws})
	model, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 50})
	m = model.(ui.Model)

	// Switch to workspace focus
	m = send(m, key(tea.KeyTab))
	if m.CurrentFocus() != ui.FocusWorkspace {
		t.Fatal("expected workspace focus after Tab")
	}

	// Press Ctrl+E — this triggers the End/CtrlE case in workspace focus
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlE})
	m = next.(ui.Model)

	// Status should show exported path
	if !strings.HasPrefix(m.Status(), "exported →") {
		t.Errorf("expected 'exported →' in status, got %q", m.Status())
	}

	// File should exist
	parts := strings.SplitN(m.Status(), "→ ", 2)
	if len(parts) == 2 {
		exportedPath := strings.TrimSpace(parts[1])
		if _, err := os.Stat(exportedPath); err != nil {
			t.Errorf("exported file not found at %q: %v", exportedPath, err)
		}
	}
}
