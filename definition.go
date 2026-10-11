package harnesssdk

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/arahe-dev/hertasdk"
)

// Definition says how to run one harness's headless CLI. It is data: the
// binary knows nothing about any particular harness.
type Definition struct {
	Name        string `json:"-"`
	Scope       string `json:"-"` // project or global
	Path        string `json:"-"`
	Description string `json:"description,omitempty"`
	// Command is the executable, resolved on PATH.
	Command string `json:"command"`
	// Args are argument groups. A group is emitted only when every
	// placeholder in it has a value, so optional flags drop out cleanly.
	Args [][]string `json:"args"`
	// Stdin is "prompt" (the default) to write the prompt to the CLI's
	// stdin; "system+prompt" to write the profile body first, as an
	// <instructions> block, for a CLI with no system-prompt option; or
	// "none".
	Stdin string `json:"stdin,omitempty"`
	// Env adds environment variables; values may use placeholders and an
	// empty result leaves the variable unset.
	Env map[string]string `json:"env,omitempty"`
	// Session is "mint" when HarnessSDK chooses the session id and passes
	// it in through {session}, {new_session} or {resume_session}.
	Session string `json:"session,omitempty"`
	// Output says where the final answer is.
	Output OutputRule `json:"output"`
	// Failures classify a failed run by matching its stderr and stdout.
	// The first match wins; anything unmatched is permanent.
	Failures []FailureRule `json:"failures,omitempty"`
}

// OutputRule locates the final answer in the CLI's stdout.
type OutputRule struct {
	// Format is "text" (all of stdout), "json" (one JSON document) or
	// "jsonl" (one JSON event per line).
	Format string `json:"format"`
	// Field is the dotted path to the answer in a JSON document.
	Field string `json:"field,omitempty"`
	// ErrorField, when truthy in the document, marks the run failed.
	ErrorField string `json:"error_field,omitempty"`
	// SessionField captures the session id from the document.
	SessionField string `json:"session_field,omitempty"`
	// Answer, Session and Error pick values out of a JSONL event stream.
	// Answer is required for jsonl; any Error match marks the run failed.
	Answer  *Selector `json:"answer,omitempty"`
	Session *Selector `json:"session,omitempty"`
	Error   *Selector `json:"error,omitempty"`
}

// Selector picks a field from the events of a JSONL stream that match
// Where (dotted path to required string value).
type Selector struct {
	Where map[string]string `json:"where,omitempty"`
	Field string            `json:"field,omitempty"`
	// Pick is "last" (default), "first" or "join" (all matches, joined).
	Pick string `json:"pick,omitempty"`
	Join string `json:"join,omitempty"`
}

// FailureRule maps a pattern in a failed run's output to an outcome.
type FailureRule struct {
	Match   string `json:"match"`
	Outcome string `json:"outcome"`
	re      *regexp.Regexp
}

// ParseDefinition decodes and validates a definition. Unknown fields are an
// error, so a misspelled key is caught when the definition is saved.
func ParseDefinition(name string, data []byte) (*Definition, error) {
	if !validName(name) {
		return nil, fmt.Errorf("harness name %q: use lower-case letters, digits, '.', '_' and '-'", name)
	}
	var d Definition
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("harness %s: %w", name, err)
	}
	d.Name = name
	if err := d.validate(); err != nil {
		return nil, fmt.Errorf("harness %s: %w", name, err)
	}
	return &d, nil
}

func (d *Definition) validate() error {
	if strings.TrimSpace(d.Command) == "" {
		return errors.New("command is required")
	}
	for i, g := range d.Args {
		if len(g) == 0 {
			return fmt.Errorf("args[%d] is an empty group", i)
		}
		for _, s := range g {
			if _, err := parseTemplate(s); err != nil {
				return fmt.Errorf("args[%d]: %w", i, err)
			}
		}
	}
	switch d.Stdin {
	case "", "prompt", "system+prompt", "none":
	default:
		return fmt.Errorf("stdin must be \"prompt\", \"system+prompt\" or \"none\", got %q", d.Stdin)
	}
	switch d.Session {
	case "", "mint":
	default:
		return fmt.Errorf("session must be \"mint\" or empty, got %q", d.Session)
	}
	for k, v := range d.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") {
			return fmt.Errorf("env key %q is invalid", k)
		}
		pieces, err := parseTemplate(v)
		if err != nil {
			return fmt.Errorf("env %s: %w", k, err)
		}
		if len(pieces) == 1 && pieces[0].splat {
			return fmt.Errorf("env %s: {*%s} is only for args", k, pieces[0].name)
		}
	}
	o := d.Output
	jsonFields := o.Field != "" || o.ErrorField != "" || o.SessionField != ""
	selectors := o.Answer != nil || o.Session != nil || o.Error != nil
	switch o.Format {
	case "text":
		if jsonFields || selectors {
			return errors.New("text output takes no fields or selectors")
		}
	case "json":
		if o.Field == "" {
			return errors.New("output.field is required for json output")
		}
		if selectors {
			return errors.New("answer, session and error selectors are for jsonl output")
		}
	case "jsonl":
		if o.Answer == nil {
			return errors.New("output.answer is required for jsonl output")
		}
		if jsonFields {
			return errors.New("field, error_field and session_field are for json output; jsonl uses answer, session and error")
		}
		for name, sel := range map[string]*Selector{"answer": o.Answer, "session": o.Session, "error": o.Error} {
			if err := sel.validate(name != "error"); err != nil {
				return fmt.Errorf("output.%s: %w", name, err)
			}
		}
	default:
		return fmt.Errorf("output.format must be \"text\", \"json\" or \"jsonl\", got %q", o.Format)
	}
	for i := range d.Failures {
		f := &d.Failures[i]
		if _, err := parseOutcome(f.Outcome); err != nil {
			return fmt.Errorf("failures[%d]: %w", i, err)
		}
		// "\b" in JSON is a backspace, not a word boundary: a control
		// character in a pattern is almost always that mistake, and the rule
		// would silently never match.
		if j := strings.IndexFunc(f.Match, func(r rune) bool { return r < 0x20 && r != '\t' }); j >= 0 {
			return fmt.Errorf("failures[%d].match has a control character (%q) at byte %d; in JSON write \\\\b for a regex word boundary, \\\\d for a digit",
				i, f.Match[j], j)
		}
		re, err := regexp.Compile(f.Match)
		if err != nil {
			return fmt.Errorf("failures[%d]: %w", i, err)
		}
		f.re = re
	}
	return nil
}

// Dropped is an argument group left out because a placeholder was empty.
type Dropped struct {
	Group   []string
	Missing string
}

// Render builds argv from the definition and vars.
func (d *Definition) Render(v Vars) (argv []string, dropped []Dropped, err error) {
	argv = []string{d.Command}
	for _, g := range d.Args {
		args, ok, missing, err := expandGroup(g, v)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			dropped = append(dropped, Dropped{Group: g, Missing: missing})
			continue
		}
		argv = append(argv, args...)
	}
	return argv, dropped, nil
}

// environ returns the extra environment as KEY=value pairs.
func (d *Definition) environ(v Vars) ([]string, error) {
	var env []string
	for k, tmpl := range d.Env {
		val, err := expandString(tmpl, v)
		if err != nil {
			return nil, err
		}
		if val != "" {
			env = append(env, k+"="+val)
		}
	}
	return env, nil
}

// classify maps a failed run's output to an outcome through the failure
// rules. An unmatched failure is permanent: absence of evidence never
// grants a retry.
func (d *Definition) classify(text string) hertasdk.Outcome {
	for _, f := range d.Failures {
		if f.re.MatchString(text) {
			o, _ := parseOutcome(f.Outcome)
			return o
		}
	}
	return hertasdk.Permanent
}

func (s *Selector) validate(needField bool) error {
	if s == nil {
		return nil
	}
	if needField && s.Field == "" {
		return errors.New("field is required")
	}
	switch s.Pick {
	case "", "last", "first", "join":
	default:
		return fmt.Errorf("pick must be last, first or join, got %q", s.Pick)
	}
	if s.Join != "" && s.Pick != "join" {
		return errors.New("join needs \"pick\": \"join\"")
	}
	return nil
}

// extract finds the answer, the failure flag and the session id in stdout.
func (d *Definition) extract(stdout []byte) (answer string, failed bool, session string, err error) {
	switch d.Output.Format {
	case "text":
		return strings.TrimRight(string(stdout), "\r\n"), false, "", nil
	case "jsonl":
		return d.extractEvents(stdout)
	}
	var doc any
	if err := json.Unmarshal(bytes.TrimSpace(stdout), &doc); err != nil {
		return "", false, "", fmt.Errorf("output is not one JSON document: %w", err)
	}
	// The failure flag and session are read first: a failure document often
	// has no answer, and its session is still worth resuming.
	if d.Output.ErrorField != "" {
		if e, ok := lookup(doc, d.Output.ErrorField); ok {
			failed = jsonTruthy(e)
		}
	}
	if d.Output.SessionField != "" {
		if s, ok := lookup(doc, d.Output.SessionField); ok {
			session = scalar(s)
		}
	}
	ans, ok := lookup(doc, d.Output.Field)
	if !ok {
		return "", failed, session, fmt.Errorf("output has no field %q", d.Output.Field)
	}
	return scalar(ans), failed, session, nil
}

// extractEvents applies the jsonl selectors. Lines that are not JSON are
// skipped: some CLIs print banners on stdout.
func (d *Definition) extractEvents(stdout []byte) (answer string, failed bool, session string, err error) {
	var events []any
	for _, line := range bytes.Split(stdout, []byte("\n")) {
		var ev any
		if json.Unmarshal(bytes.TrimSpace(line), &ev) == nil {
			events = append(events, ev)
		}
	}
	o := d.Output
	if o.Error != nil {
		if vals, matched := o.Error.pick(events); matched > 0 && (o.Error.Field == "" || len(vals) > 0) {
			failed = true
		}
	}
	if o.Session != nil {
		if vals, _ := o.Session.pick(events); len(vals) > 0 {
			session = vals[0]
		}
	}
	vals, _ := o.Answer.pick(events)
	if len(vals) == 0 {
		return "", failed, session, fmt.Errorf("no event matched output.answer in %d events", len(events))
	}
	return vals[0], failed, session, nil
}

// pick returns the selected value (one element, or none) and how many
// events matched Where.
func (s *Selector) pick(events []any) (vals []string, matched int) {
	var all []string
	for _, ev := range events {
		if !s.matches(ev) {
			continue
		}
		matched++
		if s.Field == "" {
			continue
		}
		if v, ok := lookup(ev, s.Field); ok {
			if str := scalar(v); str != "" {
				all = append(all, str)
			}
		}
	}
	if len(all) == 0 {
		return nil, matched
	}
	switch s.Pick {
	case "first":
		return all[:1], matched
	case "join":
		return []string{strings.Join(all, s.Join)}, matched
	}
	return all[len(all)-1:], matched
}

func (s *Selector) matches(ev any) bool {
	for path, want := range s.Where {
		v, ok := lookup(ev, path)
		if !ok || scalar(v) != want {
			return false
		}
	}
	return true
}

// lookup follows a dotted path through objects and array indexes.
func lookup(doc any, path string) (any, bool) {
	cur := doc
	for _, key := range strings.Split(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[key]
			if !ok {
				return nil, false
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(key)
			if err != nil || i < 0 || i >= len(node) {
				return nil, false
			}
			cur = node[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

func scalar(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

func jsonTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != "" && x != "false"
	case float64:
		return x != 0
	default:
		return true
	}
}
