package mcpbridge

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/luthor007/relay/relayd/internal/apps"
)

// The declarative vocabulary, in Go.
//
// `apps/sdk/src/ui.ts` is the same format in TypeScript and it is the one an
// app author reads. This is the one that decides, and the difference matters
// the same way it does for the manifest: the SDK's validator runs inside the
// sandbox, next to the app, where it is a convenience rather than a boundary.
// Nothing that reaches a phone is trusted because the SDK checked it.
//
// TestVocabularyDoesNotDriftFromSDK re-reads the TypeScript on every run rather
// than trusting this comment, because two implementations of one wire format is
// exactly how a phone comes to draw something the SDK swore was invalid.

// VocabularyVersion is the version stamped into every view.
//
// Bumped only when a host that understands the old version could draw a new
// view *wrongly*. Adding an optional field a host may ignore is not a bump;
// adding a block kind is, because ignoring a block loses content the app meant
// the user to see.
const VocabularyVersion = 1

// BlockKind is one of the four things an app can put in front of someone.
type BlockKind string

const (
	// KindCard is a title, an optional paragraph and some labelled values.
	KindCard BlockKind = "card"
	// KindList is an optional heading and some rows.
	KindList BlockKind = "list"
	// KindConfirm is one question and two buttons. The answer comes back on
	// SYSTEM.md §6.1's `consent.decision`, keyed by the render frame's id.
	KindConfirm BlockKind = "confirm"
	// KindSpeak is a spoken reply. The only kind that costs a permission.
	KindSpeak BlockKind = "speak"
)

// BlockKinds is the whole vocabulary.
//
// Four, matching APP-PLATFORM.md §7 word for word: "a card, a list, a
// confirmation, a spoken response". A fifth is a product decision — every host
// on both platforms grows a renderer for it, and a reviewer reading a manifest
// can no longer picture what the app will draw.
func BlockKinds() []BlockKind { return []BlockKind{KindCard, KindList, KindConfirm, KindSpeak} }

// ScopeFor is the permission a block kind costs, if any.
//
// Only speech does. Drawing on the phone of the person who installed the app
// reaches nothing of theirs — it cannot read, fetch or capture — so it is
// minted like `storage` and `log`, without a scope. Speech comes out of the
// glasses in someone's ear, which APP-PLATFORM.md §3 already sells as
// `glasses.speaker`.
func ScopeFor(k BlockKind) (apps.Scope, bool) {
	if k == KindSpeak {
		return apps.ScopeGlassesSpeaker, true
	}
	return "", false
}

// Field is one labelled value on a card.
type Field struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// Item is one row of a list.
type Item struct {
	Title    string `json:"title"`
	Subtitle string `json:"subtitle,omitempty"`
	// Detail is trailing text — a time, a count, a status.
	Detail string `json:"detail,omitempty"`
}

// Block is one element of a view. Exactly the fields for its Kind are
// meaningful, and [Block.MarshalJSON] emits only those, so a Go-built view and
// an SDK-built view serialise identically.
type Block struct {
	Kind BlockKind

	// Card and list.
	Title string
	// Card.
	Body   string
	Fields []Field
	// List.
	Items []Item
	// Confirm.
	Question     string
	ConfirmLabel string
	CancelLabel  string
	Detail       string
	// Speak.
	Text string
}

// MarshalJSON emits only the fields this kind has.
func (b Block) MarshalJSON() ([]byte, error) {
	m := map[string]any{"kind": string(b.Kind)}
	switch b.Kind {
	case KindCard:
		m["title"] = b.Title
		if b.Body != "" {
			m["body"] = b.Body
		}
		if len(b.Fields) > 0 {
			m["fields"] = b.Fields
		}
	case KindList:
		if b.Title != "" {
			m["title"] = b.Title
		}
		m["items"] = b.Items
	case KindConfirm:
		m["question"] = b.Question
		if b.ConfirmLabel != "" {
			m["confirmLabel"] = b.ConfirmLabel
		}
		if b.CancelLabel != "" {
			m["cancelLabel"] = b.CancelLabel
		}
		if b.Detail != "" {
			m["detail"] = b.Detail
		}
	case KindSpeak:
		m["text"] = b.Text
	default:
		return nil, &ViewError{Message: fmt.Sprintf("cannot marshal a %q block", b.Kind)}
	}
	return json.Marshal(m)
}

// View is what an app yields: a version and an ordered handful of blocks.
type View struct {
	Vocabulary int     `json:"vocabulary"`
	Blocks     []Block `json:"blocks"`
}

// Limits is every cap on a view, in one value, so a reviewer can read the whole
// envelope of what an app can put on a phone without reading the validator.
//
// Caps exist for the same reason the vocabulary is closed: a rendering engine
// has no caps, and something with no caps is not reviewable.
type limits struct {
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
	// Bytes caps the serialised view.
	Bytes int
}

// Limits are the caps in force. They are pinned against the SDK's LIMITS by
// TestVocabularyDoesNotDriftFromSDK.
var Limits = limits{
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

// ViewError is a view that will not render.
type ViewError struct{ Message string }

func (e *ViewError) Error() string { return "mcpbridge: " + e.Message }

func viewErrf(format string, a ...any) error {
	return &ViewError{Message: fmt.Sprintf(format, a...)}
}

// allowedKeys is the closed key set per kind. It is what keeps a host from
// having to decide whether an unrecognised field was decoration or content.
var allowedKeys = map[BlockKind][]string{
	KindCard:    {"kind", "title", "body", "fields"},
	KindList:    {"kind", "title", "items"},
	KindConfirm: {"kind", "question", "confirmLabel", "cancelLabel", "detail"},
	KindSpeak:   {"kind", "text"},
}

// FieldsFor is the closed key set for one block kind, `kind` first.
//
// Exported so TestBlockFieldsDoNotDriftFromSDK can hold it next to the SDK's
// ALLOWED_KEYS: two closed sets that have quietly stopped being the same set
// are worse than one open one, because each side believes it is authoritative.
func FieldsFor(k BlockKind) []string { return append([]string(nil), allowedKeys[k]...) }

// ParseView validates a view off the capability channel or off the wire.
//
// Strict, and strict about the version first: a view whose vocabulary this host
// does not know is refused whole. Drawing the parts a host recognises is how a
// confirmation ends up on screen with a question and no buttons.
func ParseView(data []byte) (View, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return View{}, viewErrf("a view must be a JSON object: %v", err)
	}
	for k := range raw {
		if k != "vocabulary" && k != "blocks" {
			return View{}, viewErrf("a view has \"vocabulary\" and \"blocks\"; %q is not part of the format", k)
		}
	}
	var version int
	if raw["vocabulary"] == nil || json.Unmarshal(raw["vocabulary"], &version) != nil {
		return View{}, viewErrf("a view must say which vocabulary it speaks")
	}
	if version != VocabularyVersion {
		return View{}, viewErrf(
			"this view says vocabulary %d and this host draws %d. Refusing the whole view rather "+
				"than drawing the parts it recognises: a confirmation with a question and no "+
				"buttons is worse than a screen that says the app needs a newer Relay",
			version, VocabularyVersion)
	}

	var rawBlocks []map[string]json.RawMessage
	if raw["blocks"] == nil || json.Unmarshal(raw["blocks"], &rawBlocks) != nil {
		return View{}, viewErrf("blocks must be an array of objects")
	}
	if len(rawBlocks) == 0 {
		return View{}, viewErrf("a view with no blocks renders nothing; say something or say nothing")
	}
	if len(rawBlocks) > Limits.Blocks {
		return View{}, viewErrf("a view has %d blocks; the limit is %d", len(rawBlocks), Limits.Blocks)
	}

	out := View{Vocabulary: VocabularyVersion}
	counts := map[BlockKind]int{}
	for i, rb := range rawBlocks {
		b, err := parseBlock(rb, i)
		if err != nil {
			return View{}, err
		}
		counts[b.Kind]++
		out.Blocks = append(out.Blocks, b)
	}
	if counts[KindConfirm] > 1 {
		return View{}, viewErrf("a view asks at most one question — two confirmations in one view have no defined answer")
	}
	if counts[KindSpeak] > 1 {
		return View{}, viewErrf("a view speaks at most once — two spoken blocks would talk over each other")
	}

	encoded, err := json.Marshal(out)
	if err != nil {
		return View{}, viewErrf("this view cannot be serialised: %v", err)
	}
	if len(encoded) > Limits.Bytes {
		return View{}, viewErrf("this view serialises to %d bytes; the limit is %d", len(encoded), Limits.Bytes)
	}
	return out, nil
}

// Validate re-checks a view built in Go rather than parsed. Building a view and
// sending it without validating it would let relayd emit something the SDK
// would have refused, which is the drift this package exists to prevent.
func Validate(v View) (View, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return View{}, viewErrf("this view cannot be serialised: %v", err)
	}
	return ParseView(data)
}

func parseBlock(raw map[string]json.RawMessage, i int) (Block, error) {
	var kind string
	if raw["kind"] == nil || json.Unmarshal(raw["kind"], &kind) != nil {
		return Block{}, viewErrf("blocks[%d].kind is missing", i)
	}
	k := BlockKind(kind)
	allowed, ok := allowedKeys[k]
	if !ok {
		return Block{}, viewErrf("blocks[%d].kind is %q; the vocabulary is %s", i, kind, kindList())
	}
	for key := range raw {
		if !contains(allowed, key) {
			return Block{}, viewErrf(
				"blocks[%d] is a %s with an unknown field %q. A %s draws %s and nothing else — "+
					"a field the host does not know is a field it would not draw",
				i, k, key, k, strings.Join(allowed[1:], ", "))
		}
	}

	b := Block{Kind: k}
	var err error
	switch k {
	case KindCard:
		if b.Title, err = text(raw, "title", fmt.Sprintf("blocks[%d].title", i), Limits.CardTitle, false, false); err != nil {
			return Block{}, err
		}
		if b.Body, err = text(raw, "body", fmt.Sprintf("blocks[%d].body", i), Limits.CardBody, true, true); err != nil {
			return Block{}, err
		}
		if raw["fields"] != nil {
			var rawFields []map[string]json.RawMessage
			if json.Unmarshal(raw["fields"], &rawFields) != nil {
				return Block{}, viewErrf("blocks[%d].fields must be an array of objects", i)
			}
			if len(rawFields) > Limits.CardFields {
				return Block{}, viewErrf("blocks[%d].fields has %d entries; the limit is %d", i, len(rawFields), Limits.CardFields)
			}
			for j, rf := range rawFields {
				for key := range rf {
					if key != "label" && key != "value" {
						return Block{}, viewErrf("blocks[%d].fields[%d] has an unknown field %q", i, j, key)
					}
				}
				var f Field
				if f.Label, err = text(rf, "label", fmt.Sprintf("blocks[%d].fields[%d].label", i, j), Limits.FieldLabel, false, false); err != nil {
					return Block{}, err
				}
				if f.Value, err = text(rf, "value", fmt.Sprintf("blocks[%d].fields[%d].value", i, j), Limits.FieldValue, false, false); err != nil {
					return Block{}, err
				}
				b.Fields = append(b.Fields, f)
			}
		}
	case KindList:
		if b.Title, err = text(raw, "title", fmt.Sprintf("blocks[%d].title", i), Limits.ListTitle, false, true); err != nil {
			return Block{}, err
		}
		var rawItems []map[string]json.RawMessage
		if raw["items"] == nil || json.Unmarshal(raw["items"], &rawItems) != nil {
			return Block{}, viewErrf("blocks[%d].items must be an array of objects", i)
		}
		if len(rawItems) == 0 {
			return Block{}, viewErrf("blocks[%d].items is empty — an empty list draws as a heading with nothing under it", i)
		}
		if len(rawItems) > Limits.ListItems {
			return Block{}, viewErrf("blocks[%d].items has %d entries; the limit is %d", i, len(rawItems), Limits.ListItems)
		}
		for j, ri := range rawItems {
			for key := range ri {
				if key != "title" && key != "subtitle" && key != "detail" {
					return Block{}, viewErrf("blocks[%d].items[%d] has an unknown field %q", i, j, key)
				}
			}
			var it Item
			if it.Title, err = text(ri, "title", fmt.Sprintf("blocks[%d].items[%d].title", i, j), Limits.ItemTitle, false, false); err != nil {
				return Block{}, err
			}
			if it.Subtitle, err = text(ri, "subtitle", fmt.Sprintf("blocks[%d].items[%d].subtitle", i, j), Limits.ItemSubtitle, false, true); err != nil {
				return Block{}, err
			}
			if it.Detail, err = text(ri, "detail", fmt.Sprintf("blocks[%d].items[%d].detail", i, j), Limits.ItemDetail, false, true); err != nil {
				return Block{}, err
			}
			b.Items = append(b.Items, it)
		}
	case KindConfirm:
		if b.Question, err = text(raw, "question", fmt.Sprintf("blocks[%d].question", i), Limits.Question, false, false); err != nil {
			return Block{}, err
		}
		if b.ConfirmLabel, err = text(raw, "confirmLabel", fmt.Sprintf("blocks[%d].confirmLabel", i), Limits.ButtonLabel, false, true); err != nil {
			return Block{}, err
		}
		if b.CancelLabel, err = text(raw, "cancelLabel", fmt.Sprintf("blocks[%d].cancelLabel", i), Limits.ButtonLabel, false, true); err != nil {
			return Block{}, err
		}
		if b.Detail, err = text(raw, "detail", fmt.Sprintf("blocks[%d].detail", i), Limits.ConfirmDetail, true, true); err != nil {
			return Block{}, err
		}
	case KindSpeak:
		if b.Text, err = text(raw, "text", fmt.Sprintf("blocks[%d].text", i), Limits.SpeakText, true, false); err != nil {
			return Block{}, err
		}
	}
	return b, nil
}

func kindList() string {
	names := make([]string, 0, len(BlockKinds()))
	for _, k := range BlockKinds() {
		names = append(names, string(k))
	}
	return strings.Join(names, ", ")
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// text pulls one string field out and checks it.
//
// Control characters are refused rather than stripped: a card is text a phone
// draws, not a terminal, and silently removing an escape sequence hides from
// the app that it sent something it did not mean to. Newline and tab are
// allowed only in the two fields that are paragraphs.
func text(raw map[string]json.RawMessage, key, where string, max int, multiline, optional bool) (string, error) {
	msg, present := raw[key]
	if !present || string(msg) == "null" {
		if optional {
			return "", nil
		}
		return "", viewErrf("%s is required", where)
	}
	var s string
	if err := json.Unmarshal(msg, &s); err != nil {
		return "", viewErrf("%s must be a string", where)
	}
	if strings.TrimSpace(s) == "" {
		return "", viewErrf("%s is empty — a blank %s draws a blank space", where, where)
	}
	if len([]rune(s)) > max {
		return "", viewErrf("%s is %d characters; the limit is %d", where, len([]rune(s)), max)
	}
	if !utf8.ValidString(s) {
		return "", viewErrf("%s is not valid UTF-8", where)
	}
	for _, r := range s {
		if r == '\n' || r == '\t' {
			if multiline {
				continue
			}
			return "", viewErrf("%s contains a control character; a view is text a phone draws, not a terminal", where)
		}
		if r < 0x20 || r == 0x7f {
			return "", viewErrf("%s contains a control character; a view is text a phone draws, not a terminal", where)
		}
	}
	return s, nil
}

// CheckScopes refuses a view containing a block the app was not granted.
//
// Called with what the user actually consented to, which may be narrower than
// the manifest asked for. An app that lost `glasses.speaker` at the install
// sheet does not get to speak through a view: "never emit an event you cannot
// observe" applies to the output side, and the alternative is a `speak` block
// the host silently drops.
func CheckScopes(v View, granted []apps.Scope) error {
	has := map[apps.Scope]bool{}
	for _, s := range granted {
		has[s] = true
	}
	for _, b := range v.Blocks {
		need, costs := ScopeFor(b.Kind)
		if costs && !has[need] {
			return viewErrf(
				"a %s block needs the %s permission and this app was not granted it", b.Kind, need)
		}
	}
	return nil
}

// ExpectsDecision reports whether this view asks a question somebody has to
// answer.
func ExpectsDecision(v View) bool {
	for _, b := range v.Blocks {
		if b.Kind == KindConfirm {
			return true
		}
	}
	return false
}

// ViewText is the plain-text projection of a view.
//
// Two callers, one format. An agent that called an app as an MCP tool reads
// this — it has no screen — and so does anything that has to log or narrate
// what an app put in front of someone. It is deliberately not a rendering:
// there is no attempt at columns or box drawing, because the thing that draws
// this properly is the phone. It is byte-identical to the SDK's viewText, which
// TestViewTextMatchesTheSDK pins.
func ViewText(v View) string {
	var lines []string
	for _, b := range v.Blocks {
		switch b.Kind {
		case KindCard:
			lines = append(lines, b.Title)
			if b.Body != "" {
				lines = append(lines, b.Body)
			}
			for _, f := range b.Fields {
				lines = append(lines, f.Label+": "+f.Value)
			}
		case KindList:
			if b.Title != "" {
				lines = append(lines, b.Title)
			}
			for _, it := range b.Items {
				bits := []string{it.Title}
				if it.Subtitle != "" {
					bits = append(bits, it.Subtitle)
				}
				if it.Detail != "" {
					bits = append(bits, it.Detail)
				}
				lines = append(lines, "- "+strings.Join(bits, " — "))
			}
		case KindConfirm:
			lines = append(lines, b.Question)
			if b.Detail != "" {
				lines = append(lines, b.Detail)
			}
			yes, no := b.ConfirmLabel, b.CancelLabel
			if yes == "" {
				yes = "Yes"
			}
			if no == "" {
				no = "No"
			}
			lines = append(lines, "["+yes+" / "+no+"]")
		case KindSpeak:
			lines = append(lines, b.Text)
		}
	}
	return strings.Join(lines, "\n")
}

// SpokenText is what a view says out loud, if anything. The host speaks this
// and draws the rest.
func SpokenText(v View) string {
	for _, b := range v.Blocks {
		if b.Kind == KindSpeak {
			return b.Text
		}
	}
	return ""
}

// Kinds lists the block kinds present in a view, sorted, for a log line and for
// the console's "what this app drew" column.
func Kinds(v View) []string {
	seen := map[string]bool{}
	var out []string
	for _, b := range v.Blocks {
		if seen[string(b.Kind)] {
			continue
		}
		seen[string(b.Kind)] = true
		out = append(out, string(b.Kind))
	}
	sort.Strings(out)
	return out
}
