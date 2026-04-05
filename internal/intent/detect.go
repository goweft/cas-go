// Package intent provides zero-latency intent detection for CAS messages.
// Pattern matching fires before any LLM call — no latency, deterministic.
//
// Priority order (must be preserved):
//  1. Close patterns
//  2. Self-edit exclusions — user signals they will edit manually → chat
//  3. Edit patterns
//  4. Create patterns
//  5. Chat (default)
package intent

import "regexp"

// Kind names the intent of a user message.
type Kind string

const (
	KindCreate Kind = "create_workspace"
	KindEdit   Kind = "edit_workspace"
	KindClose  Kind = "close_workspace"
	KindChat   Kind = "chat"
)

// WSType is the workspace type inferred from the message.
type WSType string

const (
	WSDocument WSType = "document"
	WSCode     WSType = "code"
	WSList     WSType = "list"
)

// Intent is the result of detecting what a message is asking for.
type Intent struct {
	Kind      Kind
	WSType    WSType
	TitleHint string
}

// ── Pattern tables ────────────────────────────────────────────────

var docNouns = `document|doc|proposal|report|letter|memo|essay|article|plan|outline|` +
	`resume|cv|email|brief|spec|story|blog|post|summary|agenda|budget|` +
	`invoice|contract|pitch|bio|profile|note|notes|` +
	`template|form|guide|handbook|manual|policy|procedure|playbook|` +
	`readme|changelog|roadmap|overview|draft|ticket|issue`

var codeNouns = `script|program|function|class|module|snippet|code|file|` +
	`api|endpoint|query|schema|migration|test|dockerfile|config`

var listNouns = `list|checklist|todo|to-do|table|inventory|index|glossary|outline`

var createVerbs = `write|draft|create|make|start|begin|compose`

// createPattern pairs a compiled regexp with the workspace type it implies.
type createPattern struct {
	re     *regexp.Regexp
	wsType WSType
}

var createPatterns []createPattern
var editPatterns []*regexp.Regexp
var selfEditPatterns []*regexp.Regexp
var closePatterns []*regexp.Regexp
var titleHintRe *regexp.Regexp

func init() {
	ci := regexp.MustCompile // alias for readability

	createPatterns = []createPattern{
		{ci(`(?i)\b(?:` + createVerbs + `)\b.*\b(?:` + codeNouns + `)\b`), WSCode},
		{ci(`(?i)\b(?:` + codeNouns + `)\b.*\b(?:` + createVerbs + `)\b`), WSCode},
		{ci(`(?i)\b(?:` + createVerbs + `)\b.*\b(?:` + listNouns + `)\b`), WSList},
		{ci(`(?i)\b(?:` + listNouns + `)\b.*\b(?:` + createVerbs + `)\b`), WSList},
		{ci(`(?i)\b(?:` + createVerbs + `)\b.*\b(?:` + docNouns + `)\b`), WSDocument},
		{ci(`(?i)\b(?:` + docNouns + `)\b.*\b(?:` + createVerbs + `)\b`), WSDocument},
		{ci(`(?i)\bnew\s+(?:document|doc|note|proposal|report|resume|email|brief)\b`), WSDocument},
		{ci(`(?i)\bnew\s+(?:script|program|code|function|module)\b`), WSCode},
		{ci(`(?i)\bnew\s+(?:list|checklist|todo)\b`), WSList},
		{ci(`(?i)\bi\s+need\s+to\s+(?:write|draft)\b`), WSDocument},
	}

	editPatterns = []*regexp.Regexp{
		ci(`(?i)\b(?:edit|update|change|modify|revise|rewrite|append|insert|remove|delete|expand|shorten|rename)\b`),
		ci(`(?i)\badd\b.{0,60}\b(?:section|paragraph|part|chapter|bullet|point|entry|item|row|function|method|class)\b`),
		ci(`(?i)\b(?:fix|improve|clean\s+up|polish|proofread|refactor|optimise|optimize)\b`),
	}

	// Checked BEFORE editPatterns — user signals manual edit → route to chat
	selfEditPatterns = []*regexp.Regexp{
		ci(`(?i)edit\s+(it\s+)?(directly|myself|yourself|manually|now)`),
		ci(`(?i)i'?ll\s+(edit|do|fix|change|update|write)\s*(it|that|this)?`),
		ci(`(?i)let\s+me\s+(edit|do|fix|change|update|write)`),
		ci(`(?i)(i'll|i will|i can)\s+(take it from here|handle it)`),
		ci(`(?i)just\s+(edit|open|show)\s+(it|the editor)`),
		ci(`(?i)i('ll| will| can)\s+do\s+(it|that)\s*(myself|manually)?`),
	}

	closePatterns = []*regexp.Regexp{
		ci(`(?i)\b(?:close|done|finish|discard|dismiss)\b.*\b(?:workspace|document|doc|editor|file)\b`),
		ci(`(?i)\b(?:workspace|document|doc|editor|file)\b.*\b(?:close|done|finish|discard|dismiss)\b`),
	}

	titleHintRe = regexp.MustCompile(
		`(?i)\b(?:write|draft|create|make|start|begin|compose)\s+(?:me\s+)?(?:a|an|the|my|our)?\s*(.+)`,
	)
}

// Detect classifies a user message and returns an Intent.
// This is the hot path — no allocations in the common case.
func Detect(message string) Intent {
	for _, re := range closePatterns {
		if re.MatchString(message) {
			return Intent{Kind: KindClose, WSType: WSDocument}
		}
	}
	for _, re := range selfEditPatterns {
		if re.MatchString(message) {
			return Intent{Kind: KindChat, WSType: WSDocument}
		}
	}
	for _, re := range editPatterns {
		if re.MatchString(message) {
			return Intent{Kind: KindEdit, WSType: WSDocument}
		}
	}
	for _, cp := range createPatterns {
		if cp.re.MatchString(message) {
			return Intent{
				Kind:      KindCreate,
				WSType:    cp.wsType,
				TitleHint: extractTitleHint(message),
			}
		}
	}
	return Intent{Kind: KindChat, WSType: WSDocument}
}

func extractTitleHint(message string) string {
	m := titleHintRe.FindStringSubmatch(message)
	if len(m) < 2 {
		words := splitWords(message, 6)
		return titleCase(words)
	}
	words := splitWords(m[1], 6)
	return titleCase(trimPunct(words))
}

func splitWords(s string, max int) []string {
	words := regexp.MustCompile(`\s+`).Split(s, -1)
	if len(words) > max {
		words = words[:max]
	}
	return words
}

func trimPunct(words []string) []string {
	if len(words) == 0 {
		return words
	}
	last := words[len(words)-1]
	last = regexp.MustCompile(`[.!?]+$`).ReplaceAllString(last, "")
	words[len(words)-1] = last
	return words
}

func titleCase(words []string) string {
	out := make([]byte, 0, 64)
	for i, w := range words {
		if i > 0 {
			out = append(out, ' ')
		}
		if len(w) > 0 {
			if w[0] >= 'a' && w[0] <= 'z' {
				out = append(out, w[0]-32)
				out = append(out, w[1:]...)
			} else {
				out = append(out, w...)
			}
		}
	}
	return string(out)
}
