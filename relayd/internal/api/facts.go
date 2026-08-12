package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
)

// DASHBOARD.md §3.3 — facts.
//
// MEMORY.md §5 *requires* this screen and calls it "what makes the whole tier
// defensible": an unexamined inference store poisons every routing decision
// downstream, silently, forever. Three of §5's five rules are visible in the
// wire types here, because a screen that cannot show them cannot be the thing
// that keeps the tier honest:
//
//   - **Every fact carries evidence, with dates.** A fact that cannot point at
//     where it came from is deleted, not kept at low confidence — so [FactView]
//     always has an Evidence field and the console can show an empty one as the
//     defect it is.
//   - **Editable, not just deletable.** A wrong fact the user can correct in
//     one field beats one they can only remove, so PATCH exists and edited_at
//     records that a human, not the extractor, wrote this.
//   - **Superseded facts are kept as history.** Contradictions replace rather
//     than accumulate, and "you used to use Firebase" is a real thing to be able
//     to answer, so the list takes a toggle rather than dropping them.
//
// There is no internal/facts package yet — extraction is M4 and
// internal/summarize deliberately proposes rather than writes. The table exists
// (store migration 0002), so this serves from it directly and returns an empty
// set rather than a 404 when there is nothing there. The screen must render on
// a box where nothing has been inferred.

// FactView is one inferred fact.
type FactView struct {
	ID        string `json:"id"`
	Subject   string `json:"subject"`
	Predicate string `json:"predicate"`
	Object    string `json:"object"`
	// Text is the sentence a human reads, and the field the edit form writes.
	Text       string  `json:"text"`
	Confidence float64 `json:"confidence"`

	FirstSeen int64 `json:"first_seen"`
	// LastSeen is what decay runs on, not creation: a long-held habit that still
	// shows up stays strong.
	LastSeen int64 `json:"last_seen"`

	SupersededBy string `json:"superseded_by,omitempty"`
	SupersededAt int64  `json:"superseded_at,omitempty"`
	Superseded   bool   `json:"superseded"`
	// Edited marks a fact a human corrected. The console shows it, because an
	// edited fact should not be quietly re-derived over the top.
	EditedAt int64 `json:"edited_at,omitempty"`

	Evidence []FactEvidence `json:"evidence"`
}

// FactEvidence is a pointer into a transcript, the same shape as the index's:
// where it came from, never a copy of it.
type FactEvidence struct {
	Runtime    string `json:"runtime"`
	Session    string `json:"session"`
	Path       string `json:"path,omitempty"`
	ByteOffset int64  `json:"byte_offset,omitempty"`
	Quote      string `json:"quote,omitempty"`
	At         int64  `json:"at"`
}

// FactList is the facts screen.
type FactList struct {
	Facts []FactView `json:"facts"`
	// Superseded is the history half, returned only when asked for, because
	// DASHBOARD.md §3.3 puts it under a toggle.
	Superseded []FactView `json:"superseded,omitempty"`
	Counts     FactCounts `json:"counts"`
	// Available is false when there is no store to read. The screen still
	// renders — an empty list beats a 404 the console has to special-case.
	Available bool   `json:"available"`
	Note      string `json:"note,omitempty"`
	At        int64  `json:"at"`
}

// FactCounts summarises the tier.
type FactCounts struct {
	Live       int `json:"live"`
	Superseded int `json:"superseded"`
	// NoEvidence is the number that cannot point at where they came from.
	// MEMORY.md §5 says those are deleted rather than kept, so anything above
	// zero is a bug in the extractor and the console should say so.
	NoEvidence int `json:"no_evidence"`
}

const noFactsNote = "no fact store on this machine yet — MEMORY.md §5's extractor lands with M4"

func (s *Server) handleFacts(w http.ResponseWriter, r *http.Request) {
	out := FactList{Facts: []FactView{}, At: s.now().UnixMilli()}
	if s.db == nil {
		out.Note = noFactsNote
		writeJSON(w, http.StatusOK, out)
		return
	}
	out.Available = true

	withSuperseded := truthy(r.URL.Query().Get("superseded"))
	limit := 500
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, CodeBadPayload, "limit must be a non-negative integer")
			return
		}
		if n > 0 {
			limit = n
		}
	}

	facts, err := s.listFacts(r.Context(), limit, withSuperseded)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeFailed, err.Error())
		return
	}
	for _, f := range facts {
		switch {
		case f.Superseded:
			out.Counts.Superseded++
			out.Superseded = append(out.Superseded, f)
		default:
			out.Counts.Live++
			out.Facts = append(out.Facts, f)
		}
		if len(f.Evidence) == 0 {
			out.Counts.NoEvidence++
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// listFacts reads the fact table and attaches evidence in one extra query.
//
// Two queries rather than a join because a fact with six pieces of evidence
// would otherwise arrive six times and have to be re-assembled, and the fact
// count here is hundreds rather than millions.
func (s *Server) listFacts(ctx context.Context, limit int, withSuperseded bool) ([]FactView, error) {
	q := `SELECT id, subject, predicate, object, text, confidence,
	             first_seen, last_seen, superseded_by, superseded_at, edited_at
	      FROM fact WHERE deleted_at IS NULL`
	if !withSuperseded {
		q += ` AND superseded_at IS NULL`
	}
	q += ` ORDER BY superseded_at IS NOT NULL, last_seen DESC LIMIT ?`

	rows, err := s.db.SQL().QueryContext(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []FactView
	byID := map[string]int{}
	for rows.Next() {
		var f FactView
		var supersededBy sql.NullString
		var supersededAt, editedAt *int64
		if err := rows.Scan(&f.ID, &f.Subject, &f.Predicate, &f.Object, &f.Text,
			&f.Confidence, &f.FirstSeen, &f.LastSeen, &supersededBy, &supersededAt, &editedAt); err != nil {
			return nil, err
		}
		f.SupersededBy = supersededBy.String
		if supersededAt != nil {
			f.SupersededAt = *supersededAt
			f.Superseded = true
		}
		if editedAt != nil {
			f.EditedAt = *editedAt
		}
		f.Evidence = []FactEvidence{}
		byID[f.ID] = len(out)
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	ev, err := s.db.SQL().QueryContext(ctx,
		`SELECT fact_id, runtime, session_id, path, byte_offset, quote, at
		 FROM fact_evidence ORDER BY at DESC`)
	if err != nil {
		return nil, err
	}
	defer ev.Close()
	for ev.Next() {
		var factID string
		var e FactEvidence
		if err := ev.Scan(&factID, &e.Runtime, &e.Session, &e.Path, &e.ByteOffset, &e.Quote, &e.At); err != nil {
			return nil, err
		}
		if i, ok := byID[factID]; ok {
			out[i].Evidence = append(out[i].Evidence, e)
		}
	}
	return out, ev.Err()
}

// ------------------------------------------------------------------- edit --

type editFactRequest struct {
	// Every field is a pointer so "set the text to empty" is distinguishable
	// from "leave the text alone". A PATCH that cannot express the difference
	// silently blanks fields the user did not touch.
	Text       *string  `json:"text,omitempty"`
	Object     *string  `json:"object,omitempty"`
	Predicate  *string  `json:"predicate,omitempty"`
	Confidence *float64 `json:"confidence,omitempty"`
}

func (s *Server) handleEditFact(w http.ResponseWriter, r *http.Request) {
	var req editFactRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadPayload, err.Error())
		return
	}
	if s.db == nil {
		writeErr(w, http.StatusServiceUnavailable, CodeUnavailable, noFactsNote)
		return
	}
	if req.Text == nil && req.Object == nil && req.Predicate == nil && req.Confidence == nil {
		writeErr(w, http.StatusBadRequest, CodeBadPayload, "nothing to change")
		return
	}
	if req.Text != nil && *req.Text == "" {
		writeErr(w, http.StatusBadRequest, CodeBadPayload,
			"a fact with no sentence is not a correction; delete it instead")
		return
	}
	if req.Confidence != nil && (*req.Confidence < 0 || *req.Confidence > 1) {
		writeErr(w, http.StatusBadRequest, CodeBadPayload, "confidence is between 0 and 1")
		return
	}

	set := ""
	var args []any
	add := func(clause string, v any) {
		if set != "" {
			set += ", "
		}
		set += clause
		args = append(args, v)
	}
	if req.Text != nil {
		add("text = ?", *req.Text)
	}
	if req.Object != nil {
		add("object = ?", *req.Object)
	}
	if req.Predicate != nil {
		add("predicate = ?", *req.Predicate)
	}
	if req.Confidence != nil {
		add("confidence = ?", *req.Confidence)
	}
	// edited_at is what tells the extractor a human has been here.
	add("edited_at = ?", s.now().UnixMilli())
	args = append(args, r.PathValue("id"))

	res, err := s.db.SQL().ExecContext(r.Context(),
		`UPDATE fact SET `+set+` WHERE id = ? AND deleted_at IS NULL`, args...)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeFailed, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, http.StatusNotFound, CodeNotFound, "no such fact")
		return
	}

	f, err := s.getFact(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeFailed, err.Error())
		return
	}
	s.publish(ConsoleEvent{Kind: ConsoleFact, Action: "edit", ID: f.ID, Outcome: "ok"})
	writeJSON(w, http.StatusOK, map[string]any{"fact": f})
}

// handleDeleteFact removes a fact from the tier without losing that it existed.
//
// Soft, because the evidence rows point at transcripts and because a fact the
// user deleted is a signal the extractor should not immediately re-derive. A
// hard delete would make "why did this come back" unanswerable.
func (s *Server) handleDeleteFact(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeErr(w, http.StatusServiceUnavailable, CodeUnavailable, noFactsNote)
		return
	}
	res, err := s.db.SQL().ExecContext(r.Context(),
		`UPDATE fact SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`,
		s.now().UnixMilli(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeFailed, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, http.StatusNotFound, CodeNotFound, "no such fact")
		return
	}
	s.publish(ConsoleEvent{Kind: ConsoleFact, Action: "delete", ID: r.PathValue("id"), Outcome: "ok"})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) getFact(ctx context.Context, id string) (FactView, error) {
	facts, err := s.listFacts(ctx, 1000, true)
	if err != nil {
		return FactView{}, err
	}
	for _, f := range facts {
		if f.ID == id {
			return f, nil
		}
	}
	return FactView{}, errors.New("no such fact")
}

func truthy(v string) bool {
	switch v {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
