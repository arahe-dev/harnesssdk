package engine

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/arahe-dev/hertasdk"
)

// Profile is a named agent: a harness, the values its definition's
// placeholders need, a Herta policy, and a system prompt.
//
// It is Markdown with a header between "---" lines. Header keys that are not
// reserved below become placeholder values; the body is the system prompt.
type Profile struct {
	Name        string
	Harness     string
	Description string
	Effect      hertasdk.Effect
	Admission   hertasdk.AdmissionMode
	Timeout     time.Duration
	Retry       hertasdk.RetryPolicy
	// Backoff is the wait before the first retry; it doubles for each
	// retry after, up to maxBackoff, with jitter.
	Backoff time.Duration
	// MaxWait bounds how long a turn waits at the gate for its budgets,
	// keys and slot; 0 takes config max_wait_minutes.
	MaxWait   time.Duration
	Budget    []hertasdk.Requirement
	Serialize string // key template; runs sharing a key run one at a time
	Vars      Vars
	System    string
	Scope     string // project or global, once loaded
	Path      string

	header []headerLine // kept in order so Set preserves the file
}

type headerLine struct {
	key, value string
	raw        string // the original line, for comments and blank lines
}

// reservedKeys configure HarnessSDK itself rather than the harness.
var reservedKeys = map[string]bool{
	"harness": true, "description": true, "effect": true, "admission": true,
	"timeout": true, "retry": true, "budget": true, "serialize": true,
	"backoff": true, "max_wait": true,
}

// builtinVars are filled in by HarnessSDK for every attempt; a profile
// cannot set them.
var builtinVars = map[string]bool{
	"prompt": true, "prompt_file": true, "system_file": true,
	"session": true, "new_session": true, "resume_session": true,
	"cwd": true, "run_id": true, "run_dir": true,
	"harness_home": true, "lineage": true,
}

// CheckVar reports whether a placeholder value may be set from outside the
// profile (harness run --set): a valid name that harness does not fill in.
func CheckVar(name string) error {
	switch {
	case !validName(name):
		return fmt.Errorf("%q is not a valid placeholder name", name)
	case builtinVars[name]:
		return fmt.Errorf("{%s} is filled in by harness and cannot be set", name)
	case reservedKeys[name]:
		return fmt.Errorf("%q is a policy key; change the profile instead", name)
	}
	return nil
}

// ParseProfile reads a profile file.
func ParseProfile(name string, data []byte) (*Profile, error) {
	if !validName(name) {
		return nil, fmt.Errorf("profile name %q: use lower-case letters, digits, '.', '_' and '-'", name)
	}
	header, body, err := splitHeader(data)
	if err != nil {
		return nil, fmt.Errorf("profile %s: %w", name, err)
	}
	p := &Profile{Name: name, Vars: Vars{}, header: header, System: strings.TrimSpace(body)}
	if err := p.load(); err != nil {
		return nil, fmt.Errorf("profile %s: %w", name, err)
	}
	return p, nil
}

func splitHeader(data []byte) ([]headerLine, string, error) {
	text := strings.ReplaceAll(string(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))), "\r\n", "\n")
	all := strings.Split(text, "\n")
	if strings.TrimSpace(all[0]) != "---" {
		return nil, "", errors.New("must start with a --- header line")
	}
	end := -1
	for i := 1; i < len(all); i++ {
		if strings.TrimSpace(all[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return nil, "", errors.New("header has no closing --- line")
	}
	body := strings.Join(all[end+1:], "\n")
	var lines []headerLine
	for _, raw := range all[1:end] {
		t := strings.TrimSpace(raw)
		if t == "" || strings.HasPrefix(t, "#") {
			lines = append(lines, headerLine{raw: raw})
			continue
		}
		k, v, ok := strings.Cut(t, ":")
		if !ok {
			return nil, "", fmt.Errorf("header line %q is not key: value", t)
		}
		lines = append(lines, headerLine{key: strings.ToLower(strings.TrimSpace(k)), value: cleanValue(v), raw: raw})
	}
	return lines, body, nil
}

// cleanValue trims a header value, drops a trailing " # comment" and one
// pair of surrounding quotes.
func cleanValue(v string) string {
	if i := strings.Index(v, " #"); i >= 0 {
		v = v[:i]
	}
	v = strings.TrimSpace(v)
	if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
		v = v[1 : len(v)-1]
	}
	return v
}

func (p *Profile) load() error {
	seen := map[string]bool{}
	for _, l := range p.header {
		if l.key == "" {
			continue
		}
		if !validName(l.key) {
			return fmt.Errorf("header key %q: use lower-case letters, digits, '.', '_' and '-'", l.key)
		}
		if seen[l.key] {
			return fmt.Errorf("header key %q appears twice", l.key)
		}
		seen[l.key] = true
		if err := p.apply(l.key, l.value); err != nil {
			return err
		}
	}
	if p.Harness == "" {
		return errors.New("header needs harness: <name>")
	}
	if p.Effect == hertasdk.EffectUnknown {
		return errors.New("header needs effect: pure, idempotent or non-idempotent (what repeating a run does)")
	}
	return nil
}

func (p *Profile) apply(key, value string) error {
	var err error
	switch key {
	case "harness":
		if !validName(value) {
			return fmt.Errorf("harness %q is not a valid name", value)
		}
		p.Harness = value
	case "description":
		p.Description = value
	case "effect":
		p.Effect, err = parseEffect(value)
	case "admission":
		p.Admission, err = parseAdmission(value)
	case "timeout":
		p.Timeout, err = parseTimeout(value)
	case "retry":
		p.Retry, err = parseRetry(value)
	case "backoff":
		p.Backoff, err = parseTimeout(value)
	case "max_wait":
		p.MaxWait, err = parseTimeout(value)
	case "budget":
		p.Budget, err = parseBudget(value)
	case "serialize":
		_, err = parseTemplate(value)
		p.Serialize = value
	default:
		if builtinVars[key] {
			return fmt.Errorf("header key %q is filled in by harness and cannot be set", key)
		}
		p.Vars[key] = value
	}
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	return nil
}

func parseEffect(s string) (hertasdk.Effect, error) {
	switch strings.ToLower(s) {
	case "pure":
		return hertasdk.Pure, nil
	case "idempotent":
		return hertasdk.Idempotent, nil
	case "non-idempotent", "nonidempotent", "non_idempotent":
		return hertasdk.NonIdempotent, nil
	}
	return 0, fmt.Errorf("%q is not pure, idempotent or non-idempotent", s)
}

func parseAdmission(s string) (hertasdk.AdmissionMode, error) {
	switch strings.ToLower(s) {
	case "", "wait":
		return hertasdk.Wait, nil
	case "reject":
		return hertasdk.Reject, nil
	}
	return 0, fmt.Errorf("%q is not wait or reject", s)
}

func parseTimeout(s string) (time.Duration, error) {
	if s == "" || s == "0" || strings.EqualFold(s, "none") {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%q is not a duration like 20m", s)
	}
	return d, nil
}

// parseRetry reads "transient x2", "transient,uncertain x3" or "none".
// Outcomes without a count mean two attempts.
func parseRetry(s string) (hertasdk.RetryPolicy, error) {
	var rp hertasdk.RetryPolicy
	if s == "" || strings.EqualFold(s, "none") {
		return rp, nil
	}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		if n, ok := strings.CutPrefix(strings.ToLower(f), "x"); ok {
			attempts, err := strconv.Atoi(n)
			if err != nil || attempts < 1 {
				return rp, fmt.Errorf("%q is not a count like x2", f)
			}
			rp.MaxAttempts = attempts
			continue
		}
		o, err := parseOutcome(f)
		if err != nil {
			return rp, err
		}
		if rp.On == nil {
			rp.On = map[hertasdk.Outcome]bool{}
		}
		rp.On[o] = true
	}
	if rp.MaxAttempts == 0 && len(rp.On) > 0 {
		rp.MaxAttempts = 2
	}
	return rp, nil
}

// parseBudget reads "pi=1, gpu=4"; a bare name claims one unit.
func parseBudget(s string) ([]hertasdk.Requirement, error) {
	var reqs []hertasdk.Requirement
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		name, units := f, int64(1)
		if n, u, ok := strings.Cut(f, "="); ok {
			v, err := strconv.ParseInt(u, 10, 64)
			if err != nil || v < 1 {
				return nil, fmt.Errorf("%q needs a positive unit count", f)
			}
			name, units = n, v
		}
		if !validName(name) {
			return nil, fmt.Errorf("budget name %q is not valid", name)
		}
		reqs = append(reqs, hertasdk.Requirement{Name: name, Units: units})
	}
	return reqs, nil
}

// Set updates header keys from key=value pairs, keeping the rest of the
// file; an empty value removes the key. The result is reparsed.
func (p *Profile) Set(pairs []string) (*Profile, error) {
	lines := append([]headerLine(nil), p.header...)
	for _, kv := range pairs {
		k, v, ok := strings.Cut(kv, "=")
		k = strings.ToLower(strings.TrimSpace(k))
		if !ok || !validName(k) {
			return nil, fmt.Errorf("%q is not key=value", kv)
		}
		v = strings.TrimSpace(v)
		i := indexKey(lines, k)
		switch {
		case v == "" && i >= 0:
			lines = append(lines[:i], lines[i+1:]...)
		case v == "":
		case i >= 0:
			lines[i] = headerLine{key: k, value: v, raw: k + ": " + v}
		default:
			lines = append(lines, headerLine{key: k, value: v, raw: k + ": " + v})
		}
	}
	return ParseProfile(p.Name, render(lines, p.System))
}

func indexKey(lines []headerLine, key string) int {
	for i, l := range lines {
		if l.key == key {
			return i
		}
	}
	return -1
}

// Bytes renders the profile file.
func (p *Profile) Bytes() []byte { return render(p.header, p.System) }

func render(lines []headerLine, system string) []byte {
	var b strings.Builder
	b.WriteString("---\n")
	for _, l := range lines {
		b.WriteString(l.raw)
		b.WriteString("\n")
	}
	b.WriteString("---\n")
	if system != "" {
		b.WriteString(system)
		b.WriteString("\n")
	}
	return []byte(b.String())
}
