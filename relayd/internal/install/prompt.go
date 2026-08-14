package install

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/term"
)

// The prompt seam. Every question the installer asks goes through it, which is
// what lets the whole flow — detection, voice, models, MCP, boot registration —
// run end to end in a test on a machine with none of the five runtimes on it.
//
// Questions carry stable ids so a scripted run answers them by name. A [Script]
// that meets an id it has no answer for fails rather than guessing, so adding a
// question to the flow without deciding what a test should say about it breaks
// the build instead of silently taking a default.

// ErrBack is what a question returns when the user asked to go back to the
// previous one instead of answering it.
//
// A question only offers it when its Back field is set, because a step that
// cannot unwind must not be able to receive this: an unhandled ErrBack would
// end the run at the exact moment the user asked for a smaller correction than
// that. Callers that set Back handle it.
var ErrBack = errors.New("install: go back")

// Choice is one row of a menu.
type Choice struct {
	ID    string
	Label string
	// Hint is the one line under the row — for a vendor group, the auth methods
	// behind it.
	Hint string
	// Risk is a warning attached to the row. ORCHESTRATOR.md §2b: risk is a
	// hint on the row, not a wall. The option exists, the warning is attached,
	// and the user decides. Inform, do not paternalise, do not quietly omit.
	Risk string
	// Recommended marks the shortest path.
	Recommended bool
	// Note is a paragraph printed once the row is chosen.
	Note string
	// Last sorts to the bottom. Custom Provider is always the last row, so the
	// list is never a cage.
	Last bool
}

// Question is a menu.
type Question struct {
	// ID is stable and is what a scripted run answers by.
	ID    string
	Title string
	// Body is the paragraph above the menu.
	Body    string
	Choices []Choice
	// Default is the choice id taken when the user just presses return.
	Default string
	// Back offers a way back to the previous question. See [ErrBack].
	Back bool
}

// ordered returns the choices with Last rows at the bottom, order otherwise
// preserved.
func (q Question) ordered() []Choice {
	out := append([]Choice(nil), q.Choices...)
	sort.SliceStable(out, func(i, j int) bool { return !out[i].Last && out[j].Last })
	return out
}

func (q Question) find(id string) (Choice, bool) {
	for _, c := range q.Choices {
		if c.ID == id {
			return c, true
		}
	}
	return Choice{}, false
}

// Confirm is a yes/no question.
type Confirm struct {
	ID      string
	Prompt  string
	Body    string
	Default bool
	// Back offers a way back to the previous question. See [ErrBack].
	Back bool
}

// Input is a free-text question.
type Input struct {
	ID      string
	Prompt  string
	Body    string
	Default string
	// Secret turns terminal echo off. It is used only for the one path where a
	// secret is typed at all — everything else is a reference, which is the
	// point of ORCHESTRATOR.md §2's "credentials are stored as references".
	Secret bool
	// Optional allows an empty answer.
	Optional bool
	// Back offers a way back to the previous question. See [ErrBack]. It is
	// never set on a secret, where a short answer is a short key and not a word
	// with a meaning.
	Back bool
}

// Prompter is the installer's whole interface to a human.
type Prompter interface {
	// Say prints a line.
	Say(format string, args ...any)
	// Section starts a step, with a heading and an optional paragraph.
	Section(title, body string)
	// Select asks a menu and returns the chosen id.
	Select(q Question) (string, error)
	Confirm(q Confirm) (bool, error)
	Input(q Input) (string, error)

	// Interactive reports whether there is somebody there to answer a question
	// that was not planned for.
	//
	// It exists for the verify/repair loop and nothing else. Every other
	// question in the installer is asked exactly once, so a prompter that takes
	// defaults answers it and moves on; a loop is the one shape where "take the
	// default" and "ask again" are the same instruction forever. [Auto] is the
	// only implementation that returns false.
	Interactive() bool
}

// ---------------------------------------------------------------- terminal

// Terminal is the interactive prompter.
type Terminal struct {
	In  io.Reader
	Out io.Writer
	// SecretFD is the file descriptor to turn echo off on. Zero means stdin.
	SecretFD int

	reader *bufio.Reader
}

// NewTerminal wires a prompter to stdin and stdout.
func NewTerminal() *Terminal {
	return &Terminal{In: os.Stdin, Out: os.Stdout, SecretFD: int(os.Stdin.Fd())}
}

func (t *Terminal) w() io.Writer {
	if t.Out == nil {
		return os.Stdout
	}
	return t.Out
}

func (t *Terminal) r() *bufio.Reader {
	if t.reader == nil {
		in := t.In
		if in == nil {
			in = os.Stdin
		}
		t.reader = bufio.NewReader(in)
	}
	return t.reader
}

// Interactive is true: there is a person at the other end of it.
func (t *Terminal) Interactive() bool { return true }

func (t *Terminal) Say(format string, args ...any) {
	fmt.Fprintf(t.w(), format+"\n", args...)
}

func (t *Terminal) Section(title, body string) {
	fmt.Fprintf(t.w(), "\n\033[1m%s\033[0m\n", title)
	if body != "" {
		fmt.Fprintf(t.w(), "%s\n", wrap(body, 76))
	}
}

func (t *Terminal) Select(q Question) (string, error) {
	if q.Title != "" {
		t.Section(q.Title, q.Body)
	} else if q.Body != "" {
		fmt.Fprintf(t.w(), "%s\n", wrap(q.Body, 76))
	}
	choices := q.ordered()
	for i, c := range choices {
		label := c.Label
		if c.Recommended {
			label += "  (recommended)"
		}
		fmt.Fprintf(t.w(), "  %2d. %s\n", i+1, label)
		if c.Hint != "" {
			fmt.Fprintf(t.w(), "      %s\n", c.Hint)
		}
		if c.Risk != "" {
			// Attached to the row, never a wall in front of it.
			fmt.Fprintf(t.w(), "      ! %s\n", c.Risk)
		}
	}
	if q.Back {
		// A row rather than a footnote: it is one of the things that can be
		// chosen here, and it is discovered the same way the others are.
		fmt.Fprintf(t.w(), "   b. Go back\n")
	}
	def := q.Default
	if def == "" && len(choices) > 0 {
		def = choices[0].ID
	}
	defIdx := 1
	for i, c := range choices {
		if c.ID == def {
			defIdx = i + 1
		}
	}
	for {
		fmt.Fprintf(t.w(), "  [%d] > ", defIdx)
		line, err := t.readLine()
		if err != nil {
			return "", err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			return def, nil
		}
		if q.Back && isBack(line) {
			return "", ErrBack
		}
		if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(choices) {
			return choices[n-1].ID, nil
		}
		if _, ok := q.find(line); ok {
			return line, nil
		}
		fmt.Fprintf(t.w(), "  not one of the options\n")
	}
}

func (t *Terminal) Confirm(c Confirm) (bool, error) {
	if c.Body != "" {
		fmt.Fprintf(t.w(), "%s\n", wrap(c.Body, 76))
	}
	suffix := "[y/N]"
	if c.Default {
		suffix = "[Y/n]"
	}
	if c.Back {
		suffix = strings.TrimSuffix(suffix, "]") + "/b]"
	}
	for {
		fmt.Fprintf(t.w(), "%s %s > ", c.Prompt, suffix)
		line, err := t.readLine()
		if err != nil {
			return false, err
		}
		answer := strings.ToLower(strings.TrimSpace(line))
		if c.Back && isBack(answer) {
			return false, ErrBack
		}
		switch answer {
		case "":
			return c.Default, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
	}
}

func (t *Terminal) Input(in Input) (string, error) {
	if in.Body != "" {
		fmt.Fprintf(t.w(), "%s\n", wrap(in.Body, 76))
	}
	back := ""
	if in.Back {
		back = " (b to go back)"
	}
	for {
		if in.Default != "" {
			fmt.Fprintf(t.w(), "%s [%s]%s > ", in.Prompt, in.Default, back)
		} else {
			fmt.Fprintf(t.w(), "%s%s > ", in.Prompt, back)
		}
		var line string
		var err error
		if in.Secret {
			line, err = t.readSecret()
		} else {
			line, err = t.readLine()
		}
		if err != nil {
			return "", err
		}
		line = strings.TrimSpace(line)
		if in.Back && isBack(line) {
			return "", ErrBack
		}
		if line == "" {
			if in.Default != "" {
				return in.Default, nil
			}
			if in.Optional {
				return "", nil
			}
			continue
		}
		return line, nil
	}
}

func (t *Terminal) readLine() (string, error) {
	line, err := t.r().ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// readSecret turns echo off where there is a terminal to turn it off on. When
// there is not — a pipe, CI — it falls back to a plain read and says so, rather
// than silently echoing a key into a scrollback buffer without warning.
func (t *Terminal) readSecret() (string, error) {
	fd := t.SecretFD
	if term.IsTerminal(fd) {
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(t.w())
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	fmt.Fprintf(t.w(), "\n  (not a terminal: this will be echoed) ")
	return t.readLine()
}

// isBack recognises the two spellings, and only where a question offered them.
func isBack(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "b", "back":
		return true
	}
	return false
}

// wrap breaks a paragraph at width, preserving blank lines.
func wrap(s string, width int) string {
	var out []string
	for _, para := range strings.Split(s, "\n") {
		if strings.TrimSpace(para) == "" {
			out = append(out, "")
			continue
		}
		line := ""
		for _, word := range strings.Fields(para) {
			switch {
			case line == "":
				line = word
			case len(line)+1+len(word) <= width:
				line += " " + word
			default:
				out = append(out, line)
				line = word
			}
		}
		if line != "" {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// ---------------------------------------------------------------- scripted

// Script answers from a map, and fails on anything it was not told about.
//
// That strictness is the point: a new question in the flow becomes a failing
// test rather than a default nobody chose.
type Script struct {
	// Answers maps a question id to a choice id, "yes"/"no", or input text.
	Answers map[string]string
	// Log is everything the installer said, in order.
	Log []string
	// Asked is every question id, in order.
	Asked []string
}

// NewScript builds a scripted prompter.
func NewScript(answers map[string]string) *Script {
	return &Script{Answers: answers}
}

// Output is everything the installer printed, joined.
func (s *Script) Output() string { return strings.Join(s.Log, "\n") }

// Interactive is true, because a script stands in for a person: a test that
// could not exercise the repair loop would be a test of a different installer.
// It answers by question id, so a loop terminates on [maxRepairAttempts].
func (s *Script) Interactive() bool { return true }

func (s *Script) Say(format string, args ...any) {
	s.Log = append(s.Log, fmt.Sprintf(format, args...))
}

func (s *Script) Section(title, body string) {
	s.Log = append(s.Log, "## "+title)
	if body != "" {
		s.Log = append(s.Log, body)
	}
}

func (s *Script) answer(id string) (string, error) {
	s.Asked = append(s.Asked, id)
	v, ok := s.Answers[id]
	if !ok {
		return "", fmt.Errorf("install: scripted run has no answer for question %q", id)
	}
	return v, nil
}

func (s *Script) Select(q Question) (string, error) {
	s.Section(q.Title, q.Body)
	for _, c := range q.ordered() {
		row := "  - " + c.ID + ": " + c.Label
		if c.Hint != "" {
			row += " — " + c.Hint
		}
		if c.Risk != "" {
			row += " ! " + c.Risk
		}
		if c.Recommended {
			row += " (recommended)"
		}
		s.Log = append(s.Log, row)
	}
	v, err := s.answer(q.ID)
	if err != nil {
		return "", err
	}
	if q.Back && isBack(v) {
		return "", ErrBack
	}
	if v == "" {
		v = q.Default
	}
	if _, ok := q.find(v); !ok {
		return "", fmt.Errorf("install: scripted answer %q is not a choice of question %q", v, q.ID)
	}
	return v, nil
}

func (s *Script) Confirm(c Confirm) (bool, error) {
	if c.Body != "" {
		s.Log = append(s.Log, c.Body)
	}
	s.Log = append(s.Log, "? "+c.Prompt)
	v, err := s.answer(c.ID)
	if err != nil {
		return false, err
	}
	if c.Back && isBack(v) {
		return false, ErrBack
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "default":
		return c.Default, nil
	case "y", "yes", "true":
		return true, nil
	case "n", "no", "false":
		return false, nil
	}
	return false, fmt.Errorf("install: %q is not a yes or a no for %q", v, c.ID)
}

func (s *Script) Input(in Input) (string, error) {
	if in.Body != "" {
		s.Log = append(s.Log, in.Body)
	}
	s.Log = append(s.Log, "? "+in.Prompt)
	v, err := s.answer(in.ID)
	if err != nil {
		return "", err
	}
	if in.Back && isBack(v) {
		return "", ErrBack
	}
	if v == "" {
		return in.Default, nil
	}
	return v, nil
}

// ---------------------------------------------------------------- automatic

// Auto takes every default without asking. It is what `relay setup --yes` uses
// on a headless box, and it is deliberately incapable of choosing anything a
// human did not already agree to by defaulting it.
type Auto struct {
	Out io.Writer
	// Yes answers every confirmation with its default rather than with yes:
	// an unattended run must not opt into anything that defaults to off.
	Log []string
}

// Interactive is false. `relay setup --yes` has nobody to ask, so a step that
// did not verify is reported and carried into the summary rather than retried
// against the same answers until the attempt cap runs out.
func (a *Auto) Interactive() bool { return false }

func (a *Auto) Say(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	a.Log = append(a.Log, line)
	if a.Out != nil {
		fmt.Fprintln(a.Out, line)
	}
}

func (a *Auto) Section(title, body string) {
	a.Say("\n%s", title)
	if body != "" {
		a.Say("%s", wrap(body, 76))
	}
}

func (a *Auto) Select(q Question) (string, error) {
	def := q.Default
	if def == "" && len(q.Choices) > 0 {
		def = q.ordered()[0].ID
	}
	a.Say("%s: %s (default)", q.Title, def)
	return def, nil
}

func (a *Auto) Confirm(c Confirm) (bool, error) {
	a.Say("%s: %v (default)", c.Prompt, c.Default)
	return c.Default, nil
}

func (a *Auto) Input(in Input) (string, error) {
	if in.Default == "" && !in.Optional {
		return "", fmt.Errorf("install: %q needs an answer and this run is unattended", in.ID)
	}
	return in.Default, nil
}
