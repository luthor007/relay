package apps

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The declarative UI vocabulary, relayd's side.
//
// `apps/sdk/src/ui.ts` says it out loud: "[checkScopes] is the enforcement on
// this side and relayd's mirror of this file is the enforcement that counts."
// This is that mirror. The SDK's copy runs inside the sandbox, in the app's own
// process, on a runner the app could in principle replace — so it is a
// convenience that gives an author a good error message while they are still
// writing, and it is not a boundary. This is the boundary.
//
// # Why it is duplicated rather than shared
//
// The same reason `config.KnownEntitlements` duplicates the routing table and
// `EmbeddingDims` duplicates the store's: the two sides are different languages
// on opposite ends of a process boundary, and the only way to share the
// definition would be to generate one from the other at build time, which is a
// toolchain we do not have and would not be able to run inside the sandbox
// anyway. What replaces sharing is [TestTheVocabularyMirrorsTheSDK], which reads
// `ui.ts` and fails when a limit, a block kind or a scope drifts. A cap that is
// 2000 here and 1500 there is not a compile error anywhere; it is an app that
// validated its own view and had it refused by the host.
//
// # What a view is, and what it deliberately is not
//
// APP-PLATFORM.md §7: "an app cannot draw arbitrary pixels on your phone. In
// exchange, it works identically on both platforms, cannot phone home with your
// data, and gets reviewed as a manifest instead of a binary." All three are
// properties of the vocabulary's size. There are four block kinds, there are no
// URLs anywhere in it, and every string is capped. A fifth kind, or one field
// holding a URL, costs one of those three.

// VocabularyVersion is the version this host draws. It must equal
// `VOCABULARY_VERSION` in the SDK.
const VocabularyVersion = 1

// BlockKind is one of the four things an app can put in front of someone.
type BlockKind string

const (
	BlockCard    BlockKind = "card"
	BlockList    BlockKind = "list"
	BlockConfirm BlockKind = "confirm"
	BlockSpeak   BlockKind = "speak"
)

// BlockKinds is the closed list, in the SDK's order.
var BlockKinds = []BlockKind{BlockCard, BlockList, BlockConfirm, BlockSpeak}

// ViewLimits mirrors the SDK's frozen LIMITS object.
//
// Every field is pinned to `ui.ts` by a test. They are a struct rather than
// loose constants so the mirror test can walk them by name.
type ViewLimits struct {
	Blocks        int
	CardTitle     int
	CardBody      int
	CardFields    int
	FieldLabel    int
	FieldValue    int
	ListTitle     int
	ListItems     int
	ItemTitle     int
	ItemSubtitle  int
	ItemDetail    int
	Question      int
	ButtonLabel   int
	ConfirmDetail int
	SpeakText     int
	// Bytes caps the serialised view. A frame larger than this is refused
	// rather than truncated: half a view is a card with no fields or a
	// confirmation with one button.
	Bytes int
}

// ViewCaps is the envelope of what an app can put on a phone.
var ViewCaps = ViewLimits{
	Blocks:        8,
	CardTitle:     120,
	CardBody:      2000,
	CardFields:    12,
	FieldLabel:    60,
	FieldValue:    240,
	ListTitle:     120,
	ListItems:     50,
	ItemTitle:     120,
	ItemSubtitle:  240,
	ItemDetail:    60,
	Question:      240,
	ButtonLabel:   32,
	ConfirmDetail: 600,
	SpeakText:     1000,
	Bytes:         16 * 1024,
}

// BlockScopes is the scope each block kind costs.
//
// Only `speak` costs one, and the reasoning is the SDK's: drawing on the phone
// of the person who installed the app reaches nothing of theirs — it cannot
// read, cannot fetch, cannot capture — so it is minted like `storage` and `log`
// are. Speech comes out of the glasses in someone's ear, and APP-PLATFORM.md §3
// already sells that as `glasses.speaker`.
var BlockScopes = map[BlockKind]Scope{
	BlockSpeak: ScopeGlassesSpeaker,
}

// Field is one labelled value on a card.
type Field struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// ListItem is one row of a list.
type ListItem struct {
	Title    string `json:"title"`
	Subtitle string `json:"subtitle,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// Block is one block of a view.
//
// One struct rather than an interface with four implementations: it crosses two
// process boundaries as JSON in both directions, and a sum type would need a
// custom unmarshaller on each side of each of them. [ParseView] is what makes
// the union safe — a card with a Question set does not survive it.
type Block struct {
	Kind BlockKind `json:"kind"`

	// card
	Title  string  `json:"title,omitempty"`
	Body   string  `json:"body,omitempty"`
	Fields []Field `json:"fields,omitempty"`

	// list — Title is shared with card, which is why ParseView checks fields
	// against the kind rather than trusting whichever ones are populated.
	Items []ListItem `json:"items,omitempty"`

	// confirm
	Question     string `json:"question,omitempty"`
	ConfirmLabel string `json:"confirmLabel,omitempty"`
	CancelLabel  string `json:"cancelLabel,omitempty"`
	Detail       string `json:"detail,omitempty"`

	// speak
	Text string `json:"text,omitempty"`
}

// View is what an app yields: a version and an ordered handful of blocks.
type View struct {
	Vocabulary int     `json:"vocabulary"`
	Blocks     []Block `json:"blocks"`
}

// ViewError is a view that will not render.
//
// It is a distinct type because the host answers it with [CodeBadRequest] and
// the app's own message, rather than [CodeFailed]: an app that sent a card with
// a 3000-character body has a bug it can fix, and telling it "failed" would
// send the author looking at the phone.
type ViewError struct{ Message string }

func (e *ViewError) Error() string { return e.Message }

func viewErr(format string, args ...any) *ViewError {
	return &ViewError{Message: fmt.Sprintf(format, args...)}
}

// allowedKeys is which JSON fields each kind may carry, mirroring the SDK's
// ALLOWED_KEYS. Go's decoder cannot tell an absent field from a zero one, so
// this is enforced over the raw object in [ParseViewJSON], before the decode
// throws the unknown key away.
var allowedKeys = map[BlockKind][]string{
	BlockCard:    {"kind", "title", "body", "fields"},
	BlockList:    {"kind", "title", "items"},
	BlockConfirm: {"kind", "question", "confirmLabel", "cancelLabel", "detail"},
	BlockSpeak:   {"kind", "text"},
}

// ParseViewJSON validates a view off the wire.
//
// It works from the raw JSON rather than only the decoded struct because a
// field that does not belong to a block's kind has to be an error and not a
// silently ignored key: an app that sets `body` on a `speak` block believes it
// will be drawn. Go's decoder discards it without a word, so the check has to
// happen before the decode.
func ParseViewJSON(raw []byte) (View, error) {
	var probe struct {
		Vocabulary *int              `json:"vocabulary"`
		Blocks     []json.RawMessage `json:"blocks"`
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&probe); err != nil {
		return View{}, viewErr("this is not a view: %v", err)
	}
	if probe.Vocabulary == nil {
		return View{}, viewErr("a view must say which vocabulary it is written in")
	}
	// Version first, and the whole view is refused rather than the parts this
	// host recognises. Partially drawing a view from the future is how a
	// confirmation ends up on screen with the wrong buttons.
	if *probe.Vocabulary != VocabularyVersion {
		return View{}, viewErr(
			"this view says vocabulary %d and this host draws %d; refusing the whole view "+
				"rather than the parts it recognises",
			*probe.Vocabulary, VocabularyVersion)
	}

	v := View{Vocabulary: VocabularyVersion}
	for i, rawBlock := range probe.Blocks {
		var keyed map[string]json.RawMessage
		if err := json.Unmarshal(rawBlock, &keyed); err != nil {
			return View{}, viewErr("blocks[%d] must be an object", i)
		}
		var kind struct {
			Kind BlockKind `json:"kind"`
		}
		_ = json.Unmarshal(rawBlock, &kind)
		allowed, ok := allowedKeys[kind.Kind]
		if !ok {
			return View{}, viewErr("blocks[%d].kind is %q; the vocabulary is %s",
				i, kind.Kind, joinKinds())
		}
		for key := range keyed {
			if !contains(allowed, key) {
				return View{}, viewErr(
					"blocks[%d] is a %s with an unknown field %q — a %s draws %s and nothing else",
					i, kind.Kind, key, kind.Kind,
					strings.Join(allowed[1:], ", "))
			}
		}
		var b Block
		if err := json.Unmarshal(rawBlock, &b); err != nil {
			return View{}, viewErr("blocks[%d] does not decode: %v", i, err)
		}
		v.Blocks = append(v.Blocks, b)
	}
	return ParseView(v)
}

// ParseView validates a decoded view.
//
// Strict in the same order as the SDK's parseView, and with the same messages
// where they are the same check, because an author who fixes an error locally
// should not meet a differently-worded version of it from the host.
func ParseView(v View) (View, error) {
	if v.Vocabulary != VocabularyVersion {
		return View{}, viewErr("this view says vocabulary %d and this host draws %d",
			v.Vocabulary, VocabularyVersion)
	}
	if len(v.Blocks) == 0 {
		return View{}, viewErr("a view with no blocks renders nothing; say something or say nothing")
	}
	if len(v.Blocks) > ViewCaps.Blocks {
		return View{}, viewErr("a view has %d blocks; the limit is %d", len(v.Blocks), ViewCaps.Blocks)
	}

	out := View{Vocabulary: VocabularyVersion, Blocks: make([]Block, 0, len(v.Blocks))}
	var confirms, speaks int
	for i, b := range v.Blocks {
		clean, err := parseBlock(b, i)
		if err != nil {
			return View{}, err
		}
		switch clean.Kind {
		case BlockConfirm:
			confirms++
		case BlockSpeak:
			speaks++
		}
		out.Blocks = append(out.Blocks, clean)
	}
	if confirms > 1 {
		return View{}, viewErr("a view asks at most one question — two confirmations in one view have no defined answer")
	}
	if speaks > 1 {
		return View{}, viewErr("a view speaks at most once — two spoken blocks would talk over each other")
	}

	encoded, err := json.Marshal(out)
	if err != nil {
		return View{}, viewErr("this view does not serialise: %v", err)
	}
	if len(encoded) > ViewCaps.Bytes {
		return View{}, viewErr("this view serialises to %d bytes; the limit is %d",
			len(encoded), ViewCaps.Bytes)
	}
	return out, nil
}

// parseBlock validates one block and returns it with every field that does not
// belong to its kind cleared.
//
// Clearing rather than only checking is what makes [Block] safe to hand on: a
// list whose Question survived would travel to the phone, where a renderer
// written against the vocabulary has no reason to expect it.
func parseBlock(b Block, i int) (Block, error) {
	at := func(field string) string { return fmt.Sprintf("blocks[%d].%s", i, field) }

	switch b.Kind {
	case BlockCard:
		out := Block{Kind: BlockCard}
		title, err := text(b.Title, at("title"), ViewCaps.CardTitle, required)
		if err != nil {
			return Block{}, err
		}
		out.Title = title
		body, err := text(b.Body, at("body"), ViewCaps.CardBody, optional, multiline)
		if err != nil {
			return Block{}, err
		}
		out.Body = body
		if len(b.Fields) > ViewCaps.CardFields {
			return Block{}, viewErr("%s has %d entries; the limit is %d",
				at("fields"), len(b.Fields), ViewCaps.CardFields)
		}
		for j, f := range b.Fields {
			label, err := text(f.Label, fmt.Sprintf("blocks[%d].fields[%d].label", i, j), ViewCaps.FieldLabel, required)
			if err != nil {
				return Block{}, err
			}
			value, err := text(f.Value, fmt.Sprintf("blocks[%d].fields[%d].value", i, j), ViewCaps.FieldValue, required)
			if err != nil {
				return Block{}, err
			}
			out.Fields = append(out.Fields, Field{Label: label, Value: value})
		}
		return out, nil

	case BlockList:
		out := Block{Kind: BlockList}
		title, err := text(b.Title, at("title"), ViewCaps.ListTitle, optional)
		if err != nil {
			return Block{}, err
		}
		out.Title = title
		if len(b.Items) == 0 {
			return Block{}, viewErr("%s is empty — an empty list draws as a heading with nothing under it", at("items"))
		}
		if len(b.Items) > ViewCaps.ListItems {
			return Block{}, viewErr("%s has %d entries; the limit is %d",
				at("items"), len(b.Items), ViewCaps.ListItems)
		}
		for j, it := range b.Items {
			row := ListItem{}
			var err error
			if row.Title, err = text(it.Title, fmt.Sprintf("blocks[%d].items[%d].title", i, j), ViewCaps.ItemTitle, required); err != nil {
				return Block{}, err
			}
			if row.Subtitle, err = text(it.Subtitle, fmt.Sprintf("blocks[%d].items[%d].subtitle", i, j), ViewCaps.ItemSubtitle, optional); err != nil {
				return Block{}, err
			}
			if row.Detail, err = text(it.Detail, fmt.Sprintf("blocks[%d].items[%d].detail", i, j), ViewCaps.ItemDetail, optional); err != nil {
				return Block{}, err
			}
			out.Items = append(out.Items, row)
		}
		return out, nil

	case BlockConfirm:
		out := Block{Kind: BlockConfirm}
		var err error
		if out.Question, err = text(b.Question, at("question"), ViewCaps.Question, required); err != nil {
			return Block{}, err
		}
		if out.ConfirmLabel, err = text(b.ConfirmLabel, at("confirmLabel"), ViewCaps.ButtonLabel, optional); err != nil {
			return Block{}, err
		}
		if out.CancelLabel, err = text(b.CancelLabel, at("cancelLabel"), ViewCaps.ButtonLabel, optional); err != nil {
			return Block{}, err
		}
		if out.Detail, err = text(b.Detail, at("detail"), ViewCaps.ConfirmDetail, optional, multiline); err != nil {
			return Block{}, err
		}
		return out, nil

	case BlockSpeak:
		txt, err := text(b.Text, at("text"), ViewCaps.SpeakText, required, multiline)
		if err != nil {
			return Block{}, err
		}
		return Block{Kind: BlockSpeak, Text: txt}, nil
	}

	return Block{}, viewErr("blocks[%d].kind is %q; the vocabulary is %s", i, b.Kind, joinKinds())
}

// textOption is how a string is allowed to be shaped.
type textOption int

const (
	required textOption = iota
	optional
	multiline
)

// text is the one string check, and the reason control characters are refused
// rather than stripped: a card is text on a phone, not a terminal. An escape
// sequence in a title has no meaning the host should try to interpret, and
// silently removing it hides from the app that it sent something it did not
// mean to.
func text(s, where string, max int, opts ...textOption) (string, error) {
	var isOptional, isMultiline bool
	for _, o := range opts {
		switch o {
		case optional:
			isOptional = true
		case multiline:
			isMultiline = true
		}
	}
	if s == "" {
		if isOptional {
			return "", nil
		}
		return "", viewErr("%s is required", where)
	}
	if strings.TrimSpace(s) == "" {
		return "", viewErr("%s is empty — a blank %s draws a blank space", where, where)
	}
	// Counted in UTF-16 code units, because that is what the SDK's
	// `String.length` counts and what the limit was written against. Counting
	// runes here would accept a view the SDK refused, and counting bytes would
	// refuse one it accepted; either way an emoji moves the boundary.
	if n := utf16Len(s); n > max {
		return "", viewErr("%s is %d characters; the limit is %d", where, n, max)
	}
	for _, r := range s {
		if r == '\n' || r == '\t' {
			if !isMultiline {
				return "", viewErr(
					"%s contains a control character; a view is text a phone draws, not a terminal", where)
			}
			continue
		}
		if r < 0x20 || r == 0x7F {
			return "", viewErr(
				"%s contains a control character; a view is text a phone draws, not a terminal", where)
		}
	}
	return s, nil
}

// utf16Len counts UTF-16 code units, matching JavaScript's String.length.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// CheckScopes refuses a view containing a block the app was not granted.
//
// Called with what the user actually consented to, which may be narrower than
// the manifest asked for. An app that lost `glasses.speaker` at the install
// sheet does not get to speak through a view — "never emit an event you cannot
// observe" applies to the output side too, and the alternative is a `speak`
// block the host silently drops, which is an app that believes it spoke.
func CheckScopes(v View, granted []Scope) error {
	for _, b := range v.Blocks {
		needed, costs := BlockScopes[b.Kind]
		if !costs {
			continue
		}
		if !hasScope(granted, needed) {
			return viewErr(
				"a %s block needs the %s permission and this app was not granted it",
				b.Kind, needed)
		}
	}
	return nil
}

// Confirm returns the view's confirmation, if it has one. ParseView has already
// established there is at most one.
func (v View) Confirm() (Block, bool) {
	for _, b := range v.Blocks {
		if b.Kind == BlockConfirm {
			return b, true
		}
	}
	return Block{}, false
}

// Text is the plain-text projection of a view, matching the SDK's viewText.
//
// Two callers, one format. An agent that called an app as an MCP tool reads
// this — it has no screen — and so does anything that has to log or narrate what
// an app put in front of someone. It is deliberately not a rendering: there is
// no attempt at columns or box drawing, because the thing that draws this
// properly is the phone.
func (v View) Text() string {
	var lines []string
	for _, b := range v.Blocks {
		switch b.Kind {
		case BlockCard:
			lines = append(lines, b.Title)
			if b.Body != "" {
				lines = append(lines, b.Body)
			}
			for _, f := range b.Fields {
				lines = append(lines, f.Label+": "+f.Value)
			}
		case BlockList:
			if b.Title != "" {
				lines = append(lines, b.Title)
			}
			for _, it := range b.Items {
				row := "- " + it.Title
				if it.Subtitle != "" {
					row += " — " + it.Subtitle
				}
				if it.Detail != "" {
					row += " (" + it.Detail + ")"
				}
				lines = append(lines, row)
			}
		case BlockConfirm:
			lines = append(lines, b.Question)
			if b.Detail != "" {
				lines = append(lines, b.Detail)
			}
		case BlockSpeak:
			lines = append(lines, b.Text)
		}
	}
	return strings.Join(lines, "\n")
}

func joinKinds() string {
	out := make([]string, 0, len(BlockKinds))
	for _, k := range BlockKinds {
		out = append(out, string(k))
	}
	return strings.Join(out, ", ")
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func hasScope(granted []Scope, want Scope) bool {
	for _, s := range granted {
		if s == want {
			return true
		}
	}
	return false
}
