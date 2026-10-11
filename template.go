package harnesssdk

import (
	"fmt"
	"strings"
)

// Vars are the values placeholders expand to. Keys are lower case.
type Vars map[string]string

// truthy reports whether name holds a value other than empty, false, 0, no
// or off. Conditional placeholders use it.
func (v Vars) truthy(name string) bool {
	switch strings.ToLower(strings.TrimSpace(v[name])) {
	case "", "false", "0", "no", "off":
		return false
	}
	return true
}

// piece is one part of a parsed template: literal text or a placeholder.
type piece struct {
	lit   string
	name  string // placeholder name, "" for a literal
	text  string // {name?text}: emitted when name is truthy
	cond  bool
	splat bool // {*name}: split on whitespace into several arguments
}

// parseTemplate parses one argument template:
//
//	{name}       the value of name
//	{name?text}  text when name is truthy, otherwise empty
//	{*name}      name's value split on whitespace, only as a whole argument
//	{{ and }}    literal braces
func parseTemplate(s string) ([]piece, error) {
	var out []piece
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			out = append(out, piece{lit: lit.String()})
			lit.Reset()
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '{' && i+1 < len(s) && s[i+1] == '{':
			lit.WriteByte('{')
			i++
		case c == '}' && i+1 < len(s) && s[i+1] == '}':
			lit.WriteByte('}')
			i++
		case c == '}':
			return nil, fmt.Errorf("template %q: unmatched }", s)
		case c == '{':
			end := strings.IndexByte(s[i:], '}')
			if end < 0 {
				return nil, fmt.Errorf("template %q: unclosed {", s)
			}
			p, err := parsePlaceholder(s[i+1 : i+end])
			if err != nil {
				return nil, fmt.Errorf("template %q: %w", s, err)
			}
			flush()
			out = append(out, p)
			i += end
		default:
			lit.WriteByte(c)
		}
	}
	flush()
	for _, p := range out {
		if p.splat && len(out) != 1 {
			return nil, fmt.Errorf("template %q: {*%s} must be the whole argument", s, p.name)
		}
	}
	return out, nil
}

func parsePlaceholder(body string) (piece, error) {
	p := piece{}
	if rest, ok := strings.CutPrefix(body, "*"); ok {
		p.splat, body = true, rest
	}
	if name, text, ok := strings.Cut(body, "?"); ok {
		if p.splat {
			return p, fmt.Errorf("{*%s} cannot be conditional", name)
		}
		p.cond, p.text, body = true, text, name
	}
	if !validName(body) {
		return p, fmt.Errorf("bad placeholder name %q", body)
	}
	p.name = body
	return p, nil
}

// expandGroup expands one argument group. A group is all or nothing: if any
// placeholder in it is empty, ok is false and missing names it.
func expandGroup(group []string, v Vars) (args []string, ok bool, missing string, err error) {
	for _, s := range group {
		pieces, err := parseTemplate(s)
		if err != nil {
			return nil, false, "", err
		}
		if len(pieces) == 1 && pieces[0].splat {
			fields := strings.Fields(v[pieces[0].name])
			if len(fields) == 0 {
				return nil, false, pieces[0].name, nil
			}
			args = append(args, fields...)
			continue
		}
		var b strings.Builder
		for _, p := range pieces {
			switch {
			case p.name == "":
				b.WriteString(p.lit)
			case p.cond:
				if !v.truthy(p.name) {
					return nil, false, p.name, nil
				}
				b.WriteString(p.text)
			default:
				val := v[p.name]
				if val == "" {
					return nil, false, p.name, nil
				}
				b.WriteString(val)
			}
		}
		args = append(args, b.String())
	}
	return args, true, "", nil
}

// expandString expands a single template, leaving empty placeholders empty.
// Profiles use it for serialize keys and definitions for env values.
func expandString(s string, v Vars) (string, error) {
	pieces, err := parseTemplate(s)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, p := range pieces {
		switch {
		case p.name == "":
			b.WriteString(p.lit)
		case p.cond:
			if v.truthy(p.name) {
				b.WriteString(p.text)
			}
		default:
			b.WriteString(v[p.name])
		}
	}
	return b.String(), nil
}

// validName reports whether s is a usable name for a placeholder, profile,
// harness, budget or variable: lower-case letters, digits, '.', '_' and '-',
// starting with a letter or digit.
func validName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case (c == '.' || c == '_' || c == '-') && i > 0:
		default:
			return false
		}
	}
	return true
}
