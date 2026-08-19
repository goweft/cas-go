package agent

import (
	"testing"

	"github.com/goweft/cas/internal/webview"
)

// Truth table for the navigation scope predicate. Integration tests cover
// the fetch-side enforcement; this pins the rule itself, including the
// cases that cannot be exercised without real DNS (public hostnames).
func TestNavigateURLInScope(t *testing.T) {
	page := &webview.PageState{
		URL: "https://cas.example/page",
		Links: []webview.Link{
			{Text: "docs", Href: "https://docs.example/guide"},
			{Text: "loopback", Href: "http://127.0.0.1:9999/x"},
			{Text: "lan", Href: "http://192.168.7.130:3080/repo"},
			{Text: "local", Href: "http://localhost:11434/api"},
			{Text: "ftp", Href: "ftp://files.example/pub"},
		},
		Text: "see also http://text-only.example/never-linked",
	}
	sess := func(start string) *webview.Session {
		s, err := webview.NewSession(t.Context(), start)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	cases := []struct {
		name   string
		start  string
		target string
		want   bool
	}{
		{"same origin", "https://cas.example/", "https://cas.example/other", true},
		{"same origin, private start chosen by user", "http://192.168.7.130:3080/", "http://192.168.7.130:3080/repo", true},
		{"public hostname link", "https://cas.example/", "https://docs.example/guide", true},
		{"linked loopback literal", "https://cas.example/", "http://127.0.0.1:9999/x", false},
		{"linked private LAN literal", "https://cas.example/", "http://192.168.7.130:3080/repo", false},
		{"linked localhost", "https://cas.example/", "http://localhost:11434/api", false},
		{"linked private literal, same hostname as start", "http://127.0.0.1:9999/", "http://127.0.0.1:9999/x", true},
		{"linked non-http scheme", "https://cas.example/", "ftp://files.example/pub", false},
		{"URL only in page text", "https://cas.example/", "http://text-only.example/never-linked", false},
		{"unlinked public URL", "https://cas.example/", "https://elsewhere.example/", false},
		{"relative URL", "https://cas.example/", "/relative/path", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := WebRequest{Session: sess(tc.start), PageState: page}
			if got := navigateURLInScope(req, tc.target); got != tc.want {
				t.Errorf("navigateURLInScope(start=%s, target=%s) = %v, want %v",
					tc.start, tc.target, got, tc.want)
			}
		})
	}
}

func TestPrivateHostLiteral(t *testing.T) {
	cases := map[string]bool{
		"localhost":       true,
		"dev.localhost":   true,
		"127.0.0.1":       true,
		"::1":             true,
		"10.1.2.3":        true,
		"192.168.0.5":     true,
		"172.16.9.9":      true,
		"169.254.169.254": true, // link-local; covers cloud metadata
		"0.0.0.0":         true,
		"docs.example":    false, // hostnames resolve at dial time
		"8.8.8.8":         false,
	}
	for host, want := range cases {
		if got := privateHostLiteral(host); got != want {
			t.Errorf("privateHostLiteral(%q) = %v, want %v", host, got, want)
		}
	}
}
