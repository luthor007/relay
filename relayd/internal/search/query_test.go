package search_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/luthor007/relay/relayd/internal/search"
)

func TestBuildMatchKeepsIdentifiersWhole(t *testing.T) {
	q, err := search.BuildMatch("where did I set STRIPE_SECRET_KEY")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// The index tokenizer splits the identifier, so the only way to ask for it
	// as a unit is a phrase. If this stops being emitted, exact-identifier
	// lookup silently becomes three independent common words.
	if !strings.Contains(q.Expr, `"stripe secret key"`) {
		t.Fatalf("no phrase for the compound identifier: %s", q.Expr)
	}
	for _, want := range []string{`"stripe"`, `"secret"`, `"key"`} {
		if !strings.Contains(q.Expr, want) {
			t.Fatalf("missing part %s in %s", want, q.Expr)
		}
	}
	// Stopwords are dropped when real terms remain.
	if strings.Contains(q.Expr, `"did"`) || strings.Contains(q.Expr, `"where"`) {
		t.Fatalf("stopwords survived: %s", q.Expr)
	}
}

func TestBuildMatchNeutralisesOperators(t *testing.T) {
	// Every one of these is FTS5 syntax. None of it may reach the parser as
	// syntax: a spoken query is user input and this is the injection boundary.
	for _, in := range []string{
		`payments AND NOT docs`,
		`stripe OR "unterminated`,
		`NEAR(a b, 3)`,
		`col:value`,
		`prefix*`,
		`^anchored`,
		`a " b "" c`,
	} {
		q, err := search.BuildMatch(in)
		if err != nil {
			if errors.Is(err, search.ErrEmptyQuery) {
				continue
			}
			t.Fatalf("%q: %v", in, err)
		}
		// Outside a quoted phrase the only thing left is our own OR separators.
		outside := stripQuoted(q.Expr)
		if strings.ContainsAny(outside, `*^:()"`) {
			t.Fatalf("%q compiled to unquoted operators: %s (outside=%q)", in, q.Expr, outside)
		}
		for _, tok := range strings.Fields(outside) {
			if tok != "OR" {
				t.Fatalf("%q left %q outside quotes: %s", in, tok, q.Expr)
			}
		}
	}
}

// stripQuoted removes every FTS5 double-quoted phrase, leaving the operators.
func stripQuoted(s string) string {
	var b strings.Builder
	in := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' {
			// A doubled quote inside a phrase is an escaped quote, not a close.
			if in && i+1 < len(s) && s[i+1] == '"' {
				i++
				continue
			}
			in = !in
			continue
		}
		if !in {
			b.WriteByte(c)
		}
	}
	return b.String()
}

func TestBuildMatchEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", "!!! ??? ---"} {
		if _, err := search.BuildMatch(in); !errors.Is(err, search.ErrEmptyQuery) {
			t.Fatalf("%q: want ErrEmptyQuery, got %v", in, err)
		}
	}
}

func TestBuildMatchAllStopwordsStillSearches(t *testing.T) {
	// "what did I do" is a real question. Refusing it would be worse than
	// answering it broadly.
	q, err := search.BuildMatch("what did I do")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if q.Expr == "" {
		t.Fatal("empty expression for an all-stopword query")
	}
}

func TestBuildMatchPathsAndHosts(t *testing.T) {
	q, err := search.BuildMatch("api.stripe.com and relay/relayd")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, want := range []string{`"api stripe com"`, `"relay relayd"`} {
		if !strings.Contains(q.Expr, want) {
			t.Fatalf("missing %s in %s", want, q.Expr)
		}
	}
}
