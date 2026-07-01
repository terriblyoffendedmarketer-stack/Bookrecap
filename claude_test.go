package main

import (
	"strings"
	"testing"
)

func TestFindTermOccurrences(t *testing.T) {
	chapters := []Chapter{
		{Index: 1, Title: "One", Text: "The SUDAR system was old. Nobody trusted the SUDAR system anymore."},
		{Index: 2, Title: "Two", Text: "No mention of that thing here."},
		{Index: 3, Title: "Three", Text: "Finally, the sudar system was explained in full detail."},
	}

	snippets := findTermOccurrences(chapters, "SUDAR system", 80)
	if len(snippets) != 3 {
		t.Fatalf("got %d snippets, want 3 (case-insensitive, all occurrences across chapters)", len(snippets))
	}
	for i, s := range snippets {
		if !strings.Contains(strings.ToLower(s), "sudar system") {
			t.Errorf("snippet %d does not contain the term: %q", i, s)
		}
	}
}

func TestFindTermOccurrences_RespectsCap(t *testing.T) {
	chapters := []Chapter{
		{Index: 1, Title: "One", Text: strings.Repeat("foo bar ", 100)},
	}
	snippets := findTermOccurrences(chapters, "foo", 5)
	if len(snippets) != 5 {
		t.Fatalf("got %d snippets, want exactly 5 (cap enforced)", len(snippets))
	}
}

func TestFindTermOccurrences_NoMatch(t *testing.T) {
	chapters := []Chapter{{Index: 1, Title: "One", Text: "nothing relevant here"}}
	if got := findTermOccurrences(chapters, "SUDAR system", 80); got != nil {
		t.Errorf("got %v, want nil for no matches", got)
	}
}

func TestFindTermOccurrences_EmptyTerm(t *testing.T) {
	chapters := []Chapter{{Index: 1, Title: "One", Text: "some text"}}
	if got := findTermOccurrences(chapters, "  ", 80); got != nil {
		t.Errorf("got %v, want nil for blank term", got)
	}
}

func TestLastUserMessageText(t *testing.T) {
	messages := []claudeMessage{
		{Role: "user", Content: "first question"},
		{Role: "assistant", Content: "an answer"},
		{Role: "user", Content: "what is a SUDAR system?"},
	}
	if got := lastUserMessageText(messages); got != "what is a SUDAR system?" {
		t.Errorf("got %q, want the latest user message", got)
	}

	if got := lastUserMessageText(nil); got != "" {
		t.Errorf("got %q, want empty for no messages", got)
	}
}
