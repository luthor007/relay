package search

import (
	"errors"
	"strings"
	"unicode"
)

// ErrEmptyQuery means the text held nothing searchable. It is returned rather
// than quietly matching everything: a spoken query that lost its content to
// transcription noise should say so, not return the ten most recent sessions and
// let the user believe they were the answer.
var ErrEmptyQuery = errors.New("search: query has no searchable terms")

// MatchQuery is a user's text compiled into FTS5 MATCH expressions.
type MatchQuery struct {
	// Raw is what the user said.
	Raw string
	// Expr is the broad FTS5 MATCH expression: every term and phrase, OR'd.
	// Every term is double-quoted, so no user text can be read as an FTS5
	// operator (AND, OR, NOT, NEAR, *, ^, :) and no user text can break the
	// expression.
	Expr string
	// ExactExpr is the narrow expression — the identifier-shaped parts of the
	// query, AND'd — or empty when the query has no identifier in it. See
	// [MatchQuery.Identifiers].
	ExactExpr string
	// Terms are the individual quoted tokens, in query order.
	Terms []string
	// Phrases are compound identifiers — STRIPE_SECRET_KEY, relay/relayd,
	// api.stripe.com — carried as FTS5 phrases so they match as units.
	Phrases []string
	// Identifiers are the parts of the query that look like a name rather than
	// a word: compound identifiers, tokens mixing letters and digits, and
	// shouted acronyms like ECONNREFUSED.
	//
	// MEMORY.md §3: exact identifiers are where dense retrieval is weakest and
	// BM25 is strongest, and they are most of what routing actually looks up.
	// When the query contains one, [Searcher] runs a third retriever that
	// returns only documents containing all of them — which is the signal that
	// keeps the one document that literally says STRIPE_SECRET_KEY above the
	// four that are merely about billing.
	Identifiers []string
}

// HasIdentifier reports whether the query named something rather than described
// it.
func (q MatchQuery) HasIdentifier() bool { return len(q.Identifiers) > 0 }

// stopwords are dropped from a query when other terms remain. Spoken queries
// carry far more of these than typed ones ("which session was the payments
// thing"), and every one of them matches thousands of summaries.
//
// The list is deliberately short. An aggressive stoplist eventually eats a term
// that mattered, and the porter tokenizer already folds inflections.
var stopwords = map[string]bool{
	"a": true, "the": true, "and": true, "or": true, "of": true,
	"to": true, "in": true, "on": true, "for": true, "with": true, "was": true,
	"is": true, "it": true, "that": true, "this": true, "which": true,
	"what": true, "where": true, "when": true, "did": true, "do": true,
	"i": true, "my": true, "me": true, "we": true, "you": true, "thing": true,
	"about": true, "from": true, "at": true, "by": true, "an": true,
	"session": true, "sessions": true,
}

// BuildMatch compiles user text into FTS5 MATCH expressions.
//
// Two things it must get right, and both are about exact identifiers:
//
//   - The index tokenizer is porter unicode61, which splits STRIPE_SECRET_KEY
//     into stripe/secret/key and keeps nothing whole. So a compound identifier
//     is emitted as the FTS5 phrase "stripe secret key", which matches those
//     three tokens adjacent and in order — as close to an exact match as the
//     tokenizer allows.
//   - Its parts are emitted too, OR'd, so a summary that says "the Stripe key"
//     is still retrieved. Losing that recall to buy precision would be a bad
//     trade when two other retrievers are already running.
func BuildMatch(text string) (MatchQuery, error) {
	q := MatchQuery{Raw: text}

	fields := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' && r != '.' && r != '/'
	})

	seenTerm := map[string]bool{}
	seenPhrase := map[string]bool{}
	seenIdent := map[string]bool{}
	var dropped []string

	addTerm := func(s string) {
		s = strings.ToLower(strings.Trim(s, "-._/"))
		if s == "" || seenTerm[s] {
			return
		}
		if stopwords[s] {
			dropped = append(dropped, s)
			return
		}
		seenTerm[s] = true
		q.Terms = append(q.Terms, s)
	}
	addIdent := func(s string) {
		if s == "" || seenIdent[s] {
			return
		}
		seenIdent[s] = true
		q.Identifiers = append(q.Identifiers, s)
	}

	shouty := allShouting(fields)

	for _, f := range fields {
		f = strings.Trim(f, "-._/")
		if f == "" {
			continue
		}
		if strings.ContainsAny(f, "_-./") {
			parts := strings.FieldsFunc(f, func(r rune) bool {
				return r == '_' || r == '-' || r == '.' || r == '/'
			})
			var clean []string
			for _, p := range parts {
				if p != "" {
					clean = append(clean, strings.ToLower(p))
				}
			}
			if len(clean) > 1 {
				phrase := strings.Join(clean, " ")
				if !seenPhrase[phrase] {
					seenPhrase[phrase] = true
					q.Phrases = append(q.Phrases, phrase)
				}
				addIdent(phrase)
			}
			for _, p := range clean {
				addTerm(p)
			}
			continue
		}
		if isIdentifierToken(f, shouty) {
			addIdent(strings.ToLower(f))
		}
		addTerm(f)
	}

	// Everything the user said was a stopword. Search it anyway rather than
	// refusing — "what did I do" is a real question, just a broad one.
	if len(q.Terms) == 0 && len(q.Phrases) == 0 {
		for _, d := range dropped {
			if !seenTerm[d] {
				seenTerm[d] = true
				q.Terms = append(q.Terms, d)
			}
		}
	}
	if len(q.Terms) == 0 && len(q.Phrases) == 0 {
		return MatchQuery{}, ErrEmptyQuery
	}

	parts := make([]string, 0, len(q.Phrases)+len(q.Terms))
	for _, p := range q.Phrases {
		parts = append(parts, quoteFTS(p))
	}
	for _, t := range q.Terms {
		parts = append(parts, quoteFTS(t))
	}
	q.Expr = strings.Join(parts, " OR ")

	if len(q.Identifiers) > 0 {
		exact := make([]string, len(q.Identifiers))
		for i, id := range q.Identifiers {
			exact[i] = quoteFTS(id)
		}
		q.ExactExpr = strings.Join(exact, " AND ")
	}
	return q, nil
}

// isIdentifierToken reports whether a single token names something rather than
// meaning something: a letters-and-digits mix (sqlite3, http2, v0.21.0) or a
// shouted acronym (ECONNREFUSED, SIGSEGV).
//
// shouty disables the acronym rule, because a query typed entirely in capitals
// is a caps-lock key, not fourteen identifiers.
func isIdentifierToken(tok string, shouty bool) bool {
	var letters, digits int
	for _, r := range tok {
		switch {
		case unicode.IsDigit(r):
			digits++
		case unicode.IsLetter(r):
			letters++
		}
	}
	if letters > 0 && digits > 0 {
		return true
	}
	if shouty || len(tok) < 4 {
		return false
	}
	return strings.ToUpper(tok) == tok && strings.ToLower(tok) != tok
}

// allShouting reports whether every alphabetic token is upper case and there is
// more than one of them.
func allShouting(fields []string) bool {
	var alpha, upper int
	for _, f := range fields {
		if strings.ToLower(f) == strings.ToUpper(f) {
			continue // no cased letters at all
		}
		alpha++
		if strings.ToUpper(f) == f {
			upper++
		}
	}
	return alpha > 1 && alpha == upper
}

// quoteFTS wraps a string in an FTS5 double-quoted phrase, doubling any
// embedded quote. Inside a phrase FTS5 treats every non-token character as a
// separator, so this neutralises the whole operator vocabulary.
func quoteFTS(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
