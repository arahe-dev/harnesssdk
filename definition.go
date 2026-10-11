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
	Description string `json:"description,omitempty"`
	// Command is the executable, resolved on PATH.
	Command string `json:"command"`
	// Args are argument groups. A group is emitted only when every
	// placeholder in it has a value, so optional flags drop out cleanly.
	Args [][]string `json:"args"`
	// Stdin is "prompt" (the default) to write the prompt to the CLI's
	// stdin, or "none".
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
	// Format is "text" (all of stdout) or "json" (one JSON document).
	Format string `json:"format"`
	// Field is the dotted path to the answer in a JSON document.
	Field string `json:"field,omitempty"`
	// ErrorField, when truthy in the document, marks the run failed.
	ErrorField string `json:"error_field,omitempty"`
	// SessionField captures the session id from the document.
	SessionField string `json:"session_field,omitempty"`
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
	case "", "prompt", "none":
	default:
		return fmt.Errorf("stdin must be \"prompt\" or \"none\", got %q", d.Stdin)
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
	switch d.Output.Format {
	case "text":
		if d.Output.Field != "" || d.Output.ErrorField != "" || d.Output.SessionField != "" {
			return errors.New("output fields need \"format\": \"json\"")
		}
	case "json":
		if d.Output.Field == "" {
			return errors.New("output.field is required for json output")
		}
	default:
		return fmt.Errorf("output.format must be \"text\" or \"json\", got %q", d.Output.Format)
	}
	for i := range d.Failures {
		f := &d.Failures[i]
		if _, err := parseOutcome(f.Outcome); err != nil {
			return fmt.Errorf("failures[%d]: %w", i, err)
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

// extract finds the answer, the failure flag and the session id in stdout.
func (d *Definition) extract(stdout []byte) (answer string, failed bool, session string, err error) {
	if d.Output.Format == "text" {
		return strings.TrimRight(string(stdout), "\r\n"), false, "", nil
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
