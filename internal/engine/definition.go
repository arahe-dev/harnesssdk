package engine

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/arahe-dev/hertasdk"
)

// Definition says how to run one harness's headless CLI. It is data: the
// binary knows nothing about any particular harness.
type Definition struct {
	// Format is the definition format, 1; omitted means 1.
	Format      int    `json:"format,omitempty"`
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
	// RetryAfter is a regex over a failed attempt's stderr and stdout whose
	// one group is how long the provider asked to wait: seconds ("30") or
	// a duration ("1m30s"). A retry waits at least that long.
	RetryAfter string `json:"retry_after,omitempty"`
	retryAfter *regexp.Regexp
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
	// Metrics are numbers recorded with each turn, such as tokens and
	// cost, by name. Each is a selector: over the events for jsonl output,
	// over the one document for json. Pick "sum" adds up every match and
	// "count" counts matches; a field may add several paths with "+".
	Metrics map[string]*Selector `json:"metrics,omitempty"`
}

// Selector picks a field from the events of a JSONL stream that match
// Where (dotted path to required string value).
type Selector struct {
	Where map[string]string `json:"where,omitempty"`
	Field string            `json:"field,omitempty"`
	// Pick is "last" (default), "first" or "join" (all matches, joined);
	// a metric may also "sum" or "count" its matches.
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
	if err := checkFormat("definition", d.Format); err != nil {
		return err
	}
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
	if len(o.Metrics) > 0 && o.Format == "text" {
		return errors.New("metrics need json or jsonl output")
	}
	for name, sel := range o.Metrics {
		if err := sel.validateMetric(name); err != nil {
			return fmt.Errorf("output.metrics.%s: %w", name, err)
		}
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
	if d.RetryAfter != "" {
		if j := strings.IndexFunc(d.RetryAfter, func(r rune) bool { return r < 0x20 && r != '\t' }); j >= 0 {
			return fmt.Errorf("retry_after has a control character (%q) at byte %d; in JSON write \\\\d for a digit", d.RetryAfter[j], j)
		}
		re, err := regexp.Compile(d.RetryAfter)
		if err != nil {
			return fmt.Errorf("retry_after: %w", err)
		}
		if re.NumSubexp() != 1 {
			return fmt.Errorf("retry_after needs exactly one group, the wait (it has %d)", re.NumSubexp())
		}
		d.retryAfter = re
	}
	return nil
}

// retryAfterIn is the wait a failed attempt's output asked for, or 0.
func (d *Definition) retryAfterIn(text string) time.Duration {
	if d.retryAfter == nil {
		return 0
	}
	m := d.retryAfter.FindStringSubmatch(text)
	if m == nil {
		return 0
	}
	if s, err := strconv.ParseFloat(strings.TrimSpace(m[1]), 64); err == nil && finite(s) && s > 0 {
		return time.Duration(min(s, maxRetryAfter.Seconds()) * float64(time.Second))
	}
	if dur, err := time.ParseDuration(strings.TrimSpace(m[1])); err == nil && dur > 0 {
		return dur
	}
	return 0
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

func (s *Selector) validateMetric(name string) error {
	switch {
	case !validName(name):
		return errors.New("use lower-case letters, digits, '.', '_' and '-'")
	case s == nil:
		return errors.New("needs a selector")
	case s.Field == "" && s.Pick != "count":
		return errors.New("field is required, except with \"pick\": \"count\"")
	case s.Join != "":
		return errors.New("join is for text values")
	}
	switch s.Pick {
	case "", "last", "first", "sum", "count":
		return nil
	}
	return fmt.Errorf("pick must be last, first, sum or count, got %q", s.Pick)
}

// extraction is what one attempt's stdout says.
type extraction struct {
	Answer  string
	Failed  bool
	Session string
	Metrics map[string]float64
}

// extract finds the answer, the failure flag, the session id and the
// metrics in stdout. The error says the answer could not be found; what
// was found is returned with it.
func (d *Definition) extract(stdout io.Reader) (extraction, error) {
	if d.Output.Format == "jsonl" {
		return d.extractEvents(stdout)
	}
	var x extraction
	data, err := io.ReadAll(stdout)
	if err != nil {
		return x, err
	}
	if d.Output.Format == "text" {
		x.Answer = strings.TrimRight(string(data), "\r\n")
		return x, nil
	}
	var doc any
	if err := json.Unmarshal(bytes.TrimSpace(data), &doc); err != nil {
		return x, fmt.Errorf("output is not one JSON document: %w", err)
	}
	// The failure flag, session and metrics are read first: a failure
	// document often has no answer, and its session is still worth resuming.
	if d.Output.ErrorField != "" {
		if e, ok := lookup(doc, d.Output.ErrorField); ok {
			x.Failed = jsonTruthy(e)
		}
	}
	if d.Output.SessionField != "" {
		if s, ok := lookup(doc, d.Output.SessionField); ok {
			x.Session = scalar(s)
		}
	}
	metrics := newMetrics(d.Output.Metrics)
	for _, c := range metrics {
		c.add(doc)
	}
	x.Metrics = metricValues(metrics)
	ans, ok := lookup(doc, d.Output.Field)
	if !ok {
		return x, fmt.Errorf("output has no field %q", d.Output.Field)
	}
	x.Answer = scalar(ans)
	return x, nil
}

// extractEvents applies the jsonl selectors to one event at a time, so a
// long stream is never held in memory. Lines that are not JSON are skipped:
// some CLIs print banners on stdout.
func (d *Definition) extractEvents(stdout io.Reader) (extraction, error) {
	var x extraction
	o := d.Output
	errs, sess, ans := newCollector(o.Error), newCollector(o.Session), newCollector(o.Answer)
	metrics := newMetrics(o.Metrics)
	cs := []*collector{errs, sess, ans}
	for _, c := range metrics {
		cs = append(cs, c)
	}
	events := 0
	br := bufio.NewReaderSize(stdout, 64<<10)
	for {
		// A line is read in place; only one longer than the buffer is
		// copied. Decoding copies what it keeps.
		line, rerr := br.ReadSlice('\n')
		if rerr == bufio.ErrBufferFull {
			long := slices.Clone(line)
			for rerr == bufio.ErrBufferFull {
				line, rerr = br.ReadSlice('\n')
				long = append(long, line...)
			}
			line = long
		}
		if line = bytes.TrimSpace(line); len(line) > 0 && slices.ContainsFunc(cs, func(c *collector) bool { return c.mayMatch(line) }) {
			var ev any
			if json.Unmarshal(line, &ev) == nil {
				events++
				for _, c := range cs {
					c.add(ev)
				}
			}
		} else if len(line) > 0 && line[0] == '{' {
			events++ // an event no selector can match
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return x, rerr
		}
	}
	if _, ok := errs.value(); errs.matched > 0 && (o.Error.Field == "" || ok) {
		x.Failed = true
	}
	x.Session, _ = sess.value()
	x.Metrics = metricValues(metrics)
	answer, ok := ans.value()
	if !ok {
		return x, fmt.Errorf("no event matched output.answer in %d events", events)
	}
	x.Answer = answer
	return x, nil
}

// collector applies one selector to a stream of events.
type collector struct {
	s       *Selector
	metric  bool
	matched int
	vals    []string // every value for pick join; otherwise the chosen one
	num     float64  // a metric's value so far
	found   bool     // a metric found a number
	// literal holds Where values that can be found in a raw line before it
	// is decoded; see mayMatch.
	literal [][]byte
}

func newCollector(s *Selector) *collector {
	c := &collector{s: s}
	if s == nil {
		return c
	}
	for _, want := range s.Where {
		if _, err := strconv.ParseFloat(want, 64); err == nil || want == "" || !literalSafe(want) {
			continue // a number may be spelled many ways; the rest may be escaped
		}
		c.literal = append(c.literal, []byte(want))
	}
	return c
}

func newMetrics(m map[string]*Selector) map[string]*collector {
	out := map[string]*collector{}
	for name, s := range m {
		c := newCollector(s)
		c.metric = true
		out[name] = c
	}
	return out
}

func metricValues(cs map[string]*collector) map[string]float64 {
	var out map[string]float64
	for name, c := range cs {
		if v, ok := c.number(); ok {
			if out == nil {
				out = map[string]float64{}
			}
			out[name] = v
		}
	}
	return out
}

// mayMatch reports whether a raw line can match, without decoding it. A
// JSON string spells a value made of literalSafe characters literally,
// unless it uses a \u escape, so a line with neither cannot match.
func (c *collector) mayMatch(line []byte) bool {
	if c.s == nil {
		return false
	}
	for _, want := range c.literal {
		if !bytes.Contains(line, want) {
			return bytes.Contains(line, []byte(`\u`))
		}
	}
	return true
}

// literalSafe reports whether a JSON string holding s must contain s
// itself, unless it uses a \u escape: s has no quote, backslash or slash
// and no control character, the characters with short escapes.
func literalSafe(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == '"' || c == '\\' || c == '/' {
			return false
		}
	}
	return true
}

func (c *collector) add(ev any) {
	if c.s == nil || !c.s.matches(ev) {
		return
	}
	c.matched++
	if c.metric {
		v, ok := numberAt(ev, c.s.Field)
		switch {
		case c.s.Pick == "count", !ok, c.s.Pick == "first" && c.found:
		case c.s.Pick == "sum":
			c.num += v
			c.found = true
		default:
			c.num, c.found = v, true
		}
		return
	}
	if c.s.Field == "" {
		return
	}
	v, ok := lookup(ev, c.s.Field)
	if !ok {
		return
	}
	str := scalar(v)
	switch {
	case str == "", c.s.Pick == "first" && len(c.vals) > 0:
	case c.s.Pick == "join":
		c.vals = append(c.vals, str)
	default:
		c.vals = append(c.vals[:0], str)
	}
}

// value is the selected value: the last match by default, the first, or
// all of them joined.
func (c *collector) value() (string, bool) {
	if len(c.vals) == 0 {
		return "", false
	}
	if c.s.Pick == "join" {
		return strings.Join(c.vals, c.s.Join), true
	}
	return c.vals[0], true
}

// number is a metric's value; a count is known even when nothing matched.
func (c *collector) number() (float64, bool) {
	if c.s.Pick == "count" {
		return float64(c.matched), true
	}
	return c.num, c.found && finite(c.num) // a sum can overflow
}

// numberAt adds up the finite numbers at one or more paths joined with "+".
// A string such as "NaN" or "Inf" parses but is not a number a record can
// hold (JSON has no such values), so it counts as missing.
func numberAt(ev any, field string) (float64, bool) {
	var sum float64
	found := false
	for path := range strings.SplitSeq(field, "+") {
		v, ok := lookup(ev, strings.TrimSpace(path))
		if !ok {
			continue
		}
		var f float64
		switch x := v.(type) {
		case float64:
			f = x
		case string:
			var err error
			if f, err = strconv.ParseFloat(x, 64); err != nil {
				continue
			}
		default:
			continue
		}
		if finite(f) {
			sum, found = sum+f, true
		}
	}
	return sum, found && finite(sum)
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// finiteMetrics drops values a record cannot hold: JSON has no NaN or
// infinity, so the record could not be saved and its turn would stay
// "running".
func finiteMetrics(m map[string]float64) map[string]float64 {
	var out map[string]float64
	for k, v := range m {
		if finite(v) {
			if out == nil {
				out = map[string]float64{}
			}
			out[k] = v
		}
	}
	return out
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

// lookup follows a dotted path through objects and array indexes; a
// negative index counts from the end.
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
			if i < 0 {
				i += len(node) // -1 is the last element
			}
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
