package config

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Change sets the value at Path, one key per level. Route names such as
// implement.S hold dots, so a dotted string couldn't name them. A nil
// Value removes the key, which puts the setting back to its default.
type Change struct {
	Path  []string `json:"path"`
	Value any      `json:"value"`
}

// Edit applies changes to a factory.yaml's bytes. It rewrites only the
// text it must, so comments, blank lines and alignment stay as they were:
// a value is replaced where it stands, a new key goes after its mapping's
// last line, and a flow mapping ({harness: claude}) stays flow. Mappings
// missing on the way are created, lists are written flow style ([a, b]),
// and a mapping left empty by a removal is removed too. The result isn't
// validated; parse it with ParseIn before saving it.
func Edit(src []byte, changes []Change) ([]byte, error) {
	out := src
	for _, c := range changes {
		if len(c.Path) == 0 || slices.Contains(c.Path, "") {
			return nil, errors.New("edit: a path needs at least one key, and no empty ones")
		}
		want, err := expected(out, c)
		if err != nil {
			return nil, err
		}
		next, err := splice(out, c)
		if err == nil && !sameDoc(next, want) {
			err = errors.New("splice changed more than the key")
		}
		if err != nil {
			// A shape splice doesn't handle (a block scalar, an anchor):
			// edit the tree and write the whole file out again.
			if next, err = treeEdit(out, c); err != nil {
				return nil, err
			}
			if !sameDoc(next, want) {
				return nil, fmt.Errorf("edit %s: could not change it", strings.Join(c.Path, "."))
			}
		}
		out = next
	}
	return out, nil
}

// --- what the edit must produce ---

// expected is the document src decodes to, with the change made.
func expected(src []byte, c Change) (any, error) {
	var doc any
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, err
	}
	if doc == nil {
		doc = map[string]any{}
	}
	root, ok := doc.(map[string]any)
	if !ok {
		return nil, errors.New("edit: the file is not a mapping")
	}
	v, err := normalize(c.Value)
	if err != nil {
		return nil, err
	}
	if v == nil {
		unset(root, c.Path)
		return root, nil
	}
	m := root
	for _, k := range c.Path[:len(c.Path)-1] {
		next, ok := m[k].(map[string]any)
		if !ok {
			if m[k] != nil {
				return nil, fmt.Errorf("edit: %s is not a mapping", k)
			}
			next = map[string]any{}
			m[k] = next
		}
		m = next
	}
	m[c.Path[len(c.Path)-1]] = v
	return root, nil
}

// unset removes path from m, and any mapping that leaves empty.
func unset(m map[string]any, path []string) {
	if len(path) == 1 {
		delete(m, path[0])
		return
	}
	sub, ok := m[path[0]].(map[string]any)
	if !ok {
		return
	}
	unset(sub, path[1:])
	if len(sub) == 0 {
		delete(m, path[0])
	}
}

// normalize is v as it reads back from YAML (2.0 is the integer 2).
func normalize(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	b, err := yaml.Marshal(map[string]any{"v": wholeNumbers(v)})
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m["v"], nil
}

// wholeNumbers turns whole floats (JSON has only floats) into integers,
// so 3 is written as 3 and not 3.0 or 3e+00.
func wholeNumbers(v any) any {
	switch x := v.(type) {
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e15 {
			return int64(x)
		}
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = wholeNumbers(e)
		}
		return out
	}
	return v
}

func sameDoc(b []byte, want any) bool {
	var got any
	if err := yaml.Unmarshal(b, &got); err != nil {
		return false
	}
	if got == nil {
		got = map[string]any{}
	}
	return reflect.DeepEqual(got, want)
}

// --- splicing text ---

// text is a file's bytes with the start of each line.
type text struct {
	b     []byte
	lines []int
}

func newText(b []byte) *text {
	t := &text{b: b, lines: []int{0}}
	for i, c := range b {
		if c == '\n' {
			t.lines = append(t.lines, i+1)
		}
	}
	return t
}

// off is the byte offset of a node's line and column (both from 1; the
// column counts characters).
func (t *text) off(line, col int) int {
	i := t.lines[line-1]
	for n := 1; n < col && i < len(t.b) && t.b[i] != '\n'; n++ {
		_, size := utf8.DecodeRune(t.b[i:])
		i += size
	}
	return i
}

func (t *text) start(n *yaml.Node) int { return t.off(n.Line, n.Column) }

// lineEnd is the offset just past the newline ending the line holding i.
func (t *text) lineEnd(i int) int {
	if j := bytes.IndexByte(t.b[i:], '\n'); j >= 0 {
		return i + j + 1
	}
	return len(t.b)
}

var errShape = errors.New("a shape splice leaves to treeEdit")

// end is the offset just past node n. flow says n is inside a flow
// collection, where a plain scalar also ends at , ] or }.
func (t *text) end(n *yaml.Node, flow bool) (int, error) {
	if n.Anchor != "" || n.Tag != "" && n.Style&yaml.TaggedStyle != 0 {
		return 0, errShape
	}
	switch n.Kind {
	case yaml.ScalarNode:
		return t.scalarEnd(n, flow)
	case yaml.MappingNode, yaml.SequenceNode:
		if n.Style&yaml.FlowStyle != 0 {
			return t.flowEnd(t.start(n))
		}
		if len(n.Content) == 0 {
			return 0, errShape
		}
		return t.end(n.Content[len(n.Content)-1], false)
	}
	return 0, errShape
}

func (t *text) scalarEnd(n *yaml.Node, flow bool) (int, error) {
	i := t.start(n)
	b := t.b
	switch {
	case n.Style&yaml.DoubleQuotedStyle != 0:
		for j := i + 1; j < len(b); j++ {
			switch b[j] {
			case '\\':
				j++
			case '"':
				return j + 1, nil
			}
		}
	case n.Style&yaml.SingleQuotedStyle != 0:
		for j := i + 1; j < len(b); j++ {
			if b[j] == '\'' {
				if j+1 < len(b) && b[j+1] == '\'' {
					j++
					continue
				}
				return j + 1, nil
			}
		}
	case n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0:
	default:
		j := i
		for ; j < len(b) && b[j] != '\n'; j++ {
			if flow && strings.IndexByte(",]}", b[j]) >= 0 || b[j] == '#' && j > i && (b[j-1] == ' ' || b[j-1] == '\t') {
				break
			}
		}
		for j > i && (b[j-1] == ' ' || b[j-1] == '\t' || b[j-1] == '\r') {
			j--
		}
		// A plain scalar folded over several lines reads differently.
		if string(b[i:j]) == n.Value || n.Tag == "!!null" && j == i {
			return j, nil
		}
	}
	return 0, errShape
}

// flowEnd is the offset just past the bracket closing the one at i.
func (t *text) flowEnd(i int) (int, error) {
	b := t.b
	if i >= len(b) || b[i] != '[' && b[i] != '{' {
		return 0, errShape
	}
	depth := 0
	for j := i; j < len(b); j++ {
		switch b[j] {
		case '[', '{':
			depth++
		case ']', '}':
			if depth--; depth == 0 {
				return j + 1, nil
			}
		case '"', '\'':
			q := b[j]
			for j++; j < len(b) && b[j] != q; j++ {
				if q == '"' && b[j] == '\\' {
					j++
				}
			}
		case '#':
			if b[j-1] == ' ' || b[j-1] == '\t' {
				for j < len(b) && b[j] != '\n' {
					j++
				}
			}
		}
	}
	return 0, errShape
}

// level is one mapping on the way down a path: the key and value found
// in it (nil when missing), and whether it is in flow style.
type level struct {
	m, key, val *yaml.Node
	flow        bool
}

func find(m *yaml.Node, k string) (key, val *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == k {
			return m.Content[i], m.Content[i+1]
		}
	}
	return nil, nil
}

// splice makes the change by rewriting only the text it touches.
func splice(src []byte, c Change) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errShape
	}
	t := newText(src)
	value, err := normalize(c.Value)
	if err != nil {
		return nil, err
	}
	var path []level
	m, flow := doc.Content[0], doc.Content[0].Style&yaml.FlowStyle != 0
	for i, k := range c.Path {
		key, val := find(m, k)
		path = append(path, level{m: m, key: key, val: val, flow: flow})
		if key == nil {
			if value == nil {
				return src, nil // nothing to remove
			}
			return t.insert(m, flow, c.Path[i:], value)
		}
		if i == len(c.Path)-1 {
			break
		}
		if val.Kind != yaml.MappingNode {
			return nil, errShape
		}
		m, flow = val, flow || val.Style&yaml.FlowStyle != 0
	}
	if value == nil {
		// Remove the key, or the highest mapping it would leave empty.
		j := len(path) - 1
		for j > 0 && len(path[j].m.Content) == 2 {
			j--
		}
		return t.remove(path[j])
	}
	return t.replace(path[len(path)-1], value)
}

func (t *text) splice(from, to int, s string) []byte {
	out := make([]byte, 0, len(t.b)+len(s))
	out = append(out, t.b[:from]...)
	out = append(out, s...)
	return append(out, t.b[to:]...)
}

// replace writes a new value over an existing one.
func (t *text) replace(l level, value any) ([]byte, error) {
	v := l.val
	s, err := render(value, l.flow)
	if err != nil {
		return nil, err
	}
	if v.Kind == yaml.MappingNode {
		return nil, errShape
	}
	from := t.start(v)
	to, err := t.end(v, l.flow)
	if err != nil {
		return nil, err
	}
	if v.Kind == yaml.SequenceNode && v.Style&yaml.FlowStyle == 0 {
		// A block list becomes a flow one, after the key's colon.
		kend, err := t.scalarEnd(l.key, l.flow)
		if err != nil {
			return nil, err
		}
		colon := bytes.IndexByte(t.b[kend:], ':')
		if colon < 0 {
			return nil, errShape
		}
		from, s = kend+colon+1, " "+s
	} else if v.Tag == "!!null" && from == to {
		// key: with nothing after it.
		s = " " + s
		if from > 0 && t.b[from-1] == ' ' {
			s = s[1:]
		}
	}
	// Keep a trailing comment where it was: take or give spaces before it.
	if !l.flow {
		sp := to
		for sp < len(t.b) && t.b[sp] == ' ' {
			sp++
		}
		if sp > to && sp < len(t.b) && t.b[sp] == '#' {
			pad := (sp - to) - (len(s) - (to - from))
			if pad < 1 {
				pad = 1
			}
			return t.splice(from, sp, s+strings.Repeat(" ", pad)), nil
		}
	}
	return t.splice(from, to, s), nil
}

// insert adds the keys of path, the last one set to value, to mapping m.
func (t *text) insert(m *yaml.Node, flow bool, path []string, value any) ([]byte, error) {
	entry := nest(path, value)
	if flow {
		open := t.start(m)
		close, err := t.flowEnd(open)
		if err != nil {
			return nil, err
		}
		close-- // the }
		styleAll(entry, yaml.FlowStyle)
		s, err := encode(entry)
		if err != nil {
			return nil, err
		}
		s = strings.TrimSpace(s)
		s = strings.TrimSuffix(strings.TrimPrefix(s, "{"), "}")
		before := bytes.TrimRight(t.b[open:close], " \t")
		at := open + len(before)
		if len(m.Content) > 0 {
			s = ", " + s
		}
		return t.splice(at, at, s), nil
	}
	if len(m.Content) == 0 {
		return nil, errShape
	}
	styleAll(entry, 0)
	s, err := encode(entry)
	if err != nil {
		return nil, err
	}
	indent := strings.Repeat(" ", m.Content[0].Column-1)
	var b strings.Builder
	if m.Content[0].Column == 1 {
		b.WriteString("\n") // a new top-level section
	}
	for _, ln := range strings.SplitAfter(strings.TrimSuffix(s, "\n"), "\n") {
		b.WriteString(indent + ln)
	}
	b.WriteString("\n")
	end, err := t.end(m, false)
	if err != nil {
		return nil, err
	}
	at := t.lineEnd(end)
	prefix := ""
	if at == len(t.b) && (len(t.b) == 0 || t.b[len(t.b)-1] != '\n') {
		prefix = "\n"
	}
	return t.splice(at, at, prefix+b.String()), nil
}

// remove deletes a key and its value.
func (t *text) remove(l level) ([]byte, error) {
	from := t.start(l.key)
	to, err := t.end(l.val, l.flow)
	if err != nil {
		return nil, err
	}
	if !l.flow {
		// Whole lines: the key's through its value's last.
		return t.splice(t.lines[l.key.Line-1], t.lineEnd(to), ""), nil
	}
	b := t.b
	j := to
	for j < len(b) && (b[j] == ' ' || b[j] == '\t') {
		j++
	}
	if j < len(b) && b[j] == ',' {
		// Not the last: take the comma after it and the space that follows.
		j++
		for j < len(b) && (b[j] == ' ' || b[j] == '\t') {
			j++
		}
		return t.splice(from, j, ""), nil
	}
	// The last: take the comma before it.
	i := from
	for i > 0 && (b[i-1] == ' ' || b[i-1] == '\t') {
		i--
	}
	if i > 0 && b[i-1] == ',' {
		i--
	}
	return t.splice(i, to, ""), nil
}

// nest builds {path[0]: {path[1]: ... value}} as a one-entry mapping.
func nest(path []string, value any) *yaml.Node {
	var v yaml.Node
	_ = v.Encode(value)
	n := &v
	for i := len(path) - 1; i >= 0; i-- {
		n = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{{Kind: yaml.ScalarNode, Tag: "!!str", Value: path[i]}, n}}
	}
	return n
}

// styleAll sets the style of n's mappings, and makes every list flow.
func styleAll(n *yaml.Node, style yaml.Style) {
	switch n.Kind {
	case yaml.MappingNode:
		n.Style = style
	case yaml.SequenceNode:
		n.Style = yaml.FlowStyle
	}
	for _, c := range n.Content {
		styleAll(c, style)
	}
}

func encode(n *yaml.Node) (string, error) {
	var b bytes.Buffer
	e := yaml.NewEncoder(&b)
	e.SetIndent(2)
	if err := e.Encode(n); err != nil {
		return "", err
	}
	if err := e.Close(); err != nil {
		return "", err
	}
	return b.String(), nil
}

// render is value written on one line, quoted where a flow collection
// needs it.
func render(value any, flow bool) (string, error) {
	var n yaml.Node
	if err := n.Encode(value); err != nil {
		return "", err
	}
	styleAll(&n, yaml.FlowStyle)
	if flow && n.Kind == yaml.ScalarNode && n.Style == 0 {
		if s, _ := encode(&n); strings.ContainsAny(s, ",[]{}#") {
			n.Style = yaml.DoubleQuotedStyle
		}
	}
	s, err := encode(&n)
	s = strings.TrimSuffix(s, "\n")
	if err == nil && strings.Contains(s, "\n") {
		err = errShape
	}
	return s, err
}

// --- editing the tree ---

// treeEdit makes the change on the parsed document and writes all of it
// out again: comments survive, but spacing and alignment may not.
func treeEdit(src []byte, c Change) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	value, err := normalize(c.Value)
	if err != nil {
		return nil, err
	}
	var stack []*yaml.Node
	m := doc.Content[0]
	for _, k := range c.Path[:len(c.Path)-1] {
		if m.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("edit: %s is not a mapping", k)
		}
		stack = append(stack, m)
		_, v := find(m, k)
		if v == nil || v.Kind == yaml.ScalarNode && v.Tag == "!!null" {
			if value == nil {
				return src, nil
			}
			nv := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Style: m.Style & yaml.FlowStyle}
			if v == nil {
				m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, nv)
				v = nv
			} else {
				*v = *nv
			}
		}
		m = v
	}
	if m.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("edit: %s is not a mapping", strings.Join(c.Path[:len(c.Path)-1], "."))
	}
	last := c.Path[len(c.Path)-1]
	if value == nil {
		drop(m, last)
		for i := len(stack) - 1; i >= 0 && len(m.Content) == 0; i-- {
			drop(stack[i], c.Path[i])
			m = stack[i]
		}
	} else {
		var nv yaml.Node
		if err := nv.Encode(value); err != nil {
			return nil, err
		}
		styleAll(&nv, yaml.FlowStyle)
		if _, v := find(m, last); v != nil {
			nv.LineComment, nv.HeadComment, nv.FootComment = v.LineComment, v.HeadComment, v.FootComment
			*v = nv
		} else {
			m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: last}, &nv)
		}
	}
	s, err := encode(&doc)
	return []byte(s), err
}

func drop(m *yaml.Node, k string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == k {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}
