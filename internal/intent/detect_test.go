package intent_test

import (
	"testing"

	"github.com/goweft/cas/internal/intent"
)

func TestDetectCreate(t *testing.T) {
	cases := []struct {
		msg    string
		wsType intent.WSType
	}{
		{"write a project proposal", intent.WSDocument},
		{"draft a resume for a software engineer", intent.WSDocument},
		{"create a python script", intent.WSCode},
		{"write a function to parse JSON", intent.WSCode},
		{"make a todo list", intent.WSList},
		{"create a checklist for deployment", intent.WSList},
	}
	for _, tc := range cases {
		t.Run(tc.msg, func(t *testing.T) {
			got := intent.Detect(tc.msg)
			if got.Kind != intent.KindCreate {
				t.Errorf("expected KindCreate, got %q", got.Kind)
			}
			if got.WSType != tc.wsType {
				t.Errorf("expected wsType %q, got %q", tc.wsType, got.WSType)
			}
		})
	}
}

func TestDetectEdit(t *testing.T) {
	cases := []string{
		"add a section about budget",
		"fix the introduction",
		"rewrite the summary",
		"improve the conclusion",
	}
	for _, msg := range cases {
		t.Run(msg, func(t *testing.T) {
			got := intent.Detect(msg)
			if got.Kind != intent.KindEdit {
				t.Errorf("expected KindEdit, got %q", got.Kind)
			}
		})
	}
}

func TestDetectSelfEdit(t *testing.T) {
	// Self-edit phrases must route to chat, not edit
	cases := []string{
		"edit it directly",
		"I'll edit it myself",
		"let me fix it",
		"I'll do that",
		"just open the editor",
	}
	for _, msg := range cases {
		t.Run(msg, func(t *testing.T) {
			got := intent.Detect(msg)
			if got.Kind != intent.KindChat {
				t.Errorf("expected KindChat (self-edit), got %q", got.Kind)
			}
		})
	}
}

func TestDetectClose(t *testing.T) {
	cases := []string{
		"close the workspace",
		"done with this document",
		"discard the editor",
	}
	for _, msg := range cases {
		t.Run(msg, func(t *testing.T) {
			got := intent.Detect(msg)
			if got.Kind != intent.KindClose {
				t.Errorf("expected KindClose, got %q", got.Kind)
			}
		})
	}
}

func TestDetectChat(t *testing.T) {
	cases := []string{
		"hello",
		"how are you",
		"what can you do",
	}
	for _, msg := range cases {
		t.Run(msg, func(t *testing.T) {
			got := intent.Detect(msg)
			if got.Kind != intent.KindChat {
				t.Errorf("expected KindChat, got %q", got.Kind)
			}
		})
	}
}

func TestTitleHint(t *testing.T) {
	got := intent.Detect("write a project proposal for Q3")
	if got.TitleHint == "" {
		t.Error("expected non-empty TitleHint")
	}
}
