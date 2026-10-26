package compose

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Mapping is a YAML mapping that remembers the declaration order of its keys.
type Mapping struct {
	Keys   []string
	Values map[string]any
}

func newMapping() *Mapping { return &Mapping{Values: map[string]any{}} }

// Get returns the value of key, or nil. It is safe on a nil Mapping.
func (m *Mapping) Get(key string) any {
	if m == nil {
		return nil
	}
	return m.Values[key]
}

// Has reports whether key is present (even with a null value).
func (m *Mapping) Has(key string) bool {
	if m == nil {
		return false
	}
	_, ok := m.Values[key]
	return ok
}

func (m *Mapping) Len() int {
	if m == nil {
		return 0
	}
	return len(m.Keys)
}

func (m *Mapping) set(key string, value any) {
	if _, ok := m.Values[key]; !ok {
		m.Keys = append(m.Keys, key)
	}
	m.Values[key] = value
}

// parseYAML decodes a document into a tree of *Mapping, []any and scalars.
// Scalars become nil, bool, int, float64 or string; anything else (timestamps,
// binary, big integers) keeps its source text, like compose does.
func parseYAML(data []byte) (any, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Kind == 0 {
		return nil, nil // empty file
	}
	d := &decoder{active: map[*yaml.Node]bool{}}
	return d.fromNode(&doc)
}

// maxNodes bounds the decoded tree, so that nested aliases ("billion laughs")
// cannot exhaust memory: decoding through yaml.Node bypasses yaml.v3's guard.
const maxNodes = 1_000_000

type decoder struct {
	nodes  int
	active map[*yaml.Node]bool // collections being decoded, to detect alias cycles
}

func (d *decoder) fromNode(n *yaml.Node) (any, error) {
	if d.nodes++; d.nodes > maxNodes {
		return nil, fmt.Errorf("line %d: document too large (more than %d nodes after alias expansion)", n.Line, maxNodes)
	}
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil, nil
		}
		return d.fromNode(n.Content[0])
	case yaml.AliasNode:
		if d.active[n.Alias] {
			return nil, fmt.Errorf("line %d: alias %q references itself", n.Line, n.Value)
		}
		return d.fromNode(n.Alias)
	case yaml.SequenceNode:
		d.active[n] = true
		defer delete(d.active, n)
		out := make([]any, 0, len(n.Content))
		for _, item := range n.Content {
			v, err := d.fromNode(item)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case yaml.MappingNode:
		d.active[n] = true
		defer delete(d.active, n)
		return d.mapping(n)
	case yaml.ScalarNode:
		return scalarFromNode(n), nil
	}
	return nil, fmt.Errorf("line %d: unsupported YAML node", n.Line)
}

// mapping decodes a mapping like PyYAML: entries from `<<:` merge keys come
// first (the last mapping of a merge sequence first), then explicit entries.
// A later entry overrides the value of an earlier one but keeps its position,
// so explicit keys win, and earlier merge sources win over later ones.
func (d *decoder) mapping(n *yaml.Node) (*Mapping, error) {
	m := newMapping()
	for i := 0; i+1 < len(n.Content); i += 2 {
		if key := n.Content[i]; key.Tag == "!!merge" {
			if err := d.merge(m, n.Content[i+1]); err != nil {
				return nil, err
			}
		}
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, value := n.Content[i], n.Content[i+1]
		if key.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("line %d: mapping keys must be scalars", key.Line)
		}
		if key.Tag == "!!merge" {
			continue
		}
		v, err := d.fromNode(value)
		if err != nil {
			return nil, err
		}
		m.set(key.Value, v)
	}
	return m, nil
}

func (d *decoder) merge(m *Mapping, value *yaml.Node) error {
	sources := []*yaml.Node{value}
	target := value
	if target.Kind == yaml.AliasNode {
		if target = target.Alias; d.active[target] {
			return fmt.Errorf("line %d: alias %q references itself", value.Line, value.Value)
		}
	}
	if target.Kind == yaml.SequenceNode {
		sources = target.Content
	}
	for i := len(sources) - 1; i >= 0; i-- {
		v, err := d.fromNode(sources[i])
		if err != nil {
			return err
		}
		merged, ok := v.(*Mapping)
		if !ok {
			return fmt.Errorf("line %d: merge key value must be a mapping", sources[i].Line)
		}
		for _, k := range merged.Keys {
			m.set(k, merged.Values[k])
		}
	}
	return nil
}

// yaml11Bool is an unquoted YAML 1.1 boolean (yes/no/on/off) that YAML 1.2
// reads as a string: it keeps its text, but is truthy like PyYAML's bool.
type yaml11Bool struct {
	text  string
	value bool
}

var yaml11Bools = map[string]bool{
	"yes": true, "Yes": true, "YES": true, "on": true, "On": true, "ON": true,
	"no": false, "No": false, "NO": false, "off": false, "Off": false, "OFF": false,
}

// pyYAMLFloatRE matches the floats of YAML 1.1 (PyYAML), which need a dot and
// a signed exponent: YAML 1.2 also reads 1e3 as a float.
var pyYAMLFloatRE = regexp.MustCompile(`^[-+]?[0-9][0-9_]*\.[0-9_]*([eE][-+][0-9]+)?$|^\.[0-9][0-9_]*([eE][-+][0-9]+)?$|^[-+]?\.(inf|Inf|INF)$|^\.(nan|NaN|NAN)$`)

// scalarFromNode decodes a scalar like PyYAML: plain scalars that only YAML
// 1.2 reads as numbers (1e3, 0o17) keep their text.
func scalarFromNode(n *yaml.Node) any {
	const notPlain = yaml.TaggedStyle | yaml.DoubleQuotedStyle | yaml.SingleQuotedStyle | yaml.LiteralStyle | yaml.FoldedStyle
	plain := n.Style&notPlain == 0
	switch n.ShortTag() {
	case "!!null":
		return nil
	case "!!str":
		if value, ok := yaml11Bools[n.Value]; ok && plain {
			return yaml11Bool{n.Value, value}
		}
	case "!!bool":
		var b bool
		if n.Decode(&b) == nil {
			return b
		}
		if value, ok := yaml11Bools[n.Value]; ok { // !!bool off
			return yaml11Bool{n.Value, value}
		}
	case "!!int":
		var i int
		if plain && strings.Contains(strings.ToLower(n.Value), "0o") {
			return n.Value
		}
		if n.Decode(&i) == nil {
			return i
		}
	case "!!float":
		var f float64
		if plain && !pyYAMLFloatRE.MatchString(n.Value) {
			return n.Value
		}
		if n.Decode(&f) == nil {
			return f
		}
	}
	return n.Value
}

// --- helpers over the generic YAML tree ------------------------------------

// scalar renders a YAML scalar as a string ("" for null).
func scalar(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case yaml11Bool:
		return x.text
	case float64:
		s := strconv.FormatFloat(x, 'f', -1, 64)
		if !strings.ContainsAny(s, ".IN") { // 1.0 stays 1.0, like Python's str
			s += ".0"
		}
		return s
	default:
		return fmt.Sprint(x)
	}
}

// truthy mirrors Python truthiness for values loaded by PyYAML: unquoted
// yes/no/on/off are booleans, any other non-empty string is true.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case yaml11Bool:
		return x.value
	case string:
		return x != ""
	case int:
		return x != 0
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case *Mapping:
		return x.Len() > 0
	}
	return true
}

// asMap returns v as a mapping; nil (or a non-mapping) gives a nil *Mapping,
// whose Get/Has are safe to call.
func asMap(v any) *Mapping {
	m, _ := v.(*Mapping)
	return m
}

// expectMap returns v as a mapping (empty for null), or fails naming what.
func expectMap(v any, what string) (*Mapping, error) {
	switch x := v.(type) {
	case nil:
		return newMapping(), nil
	case *Mapping:
		return x, nil
	}
	return nil, errorf("%s: expected a mapping, got %s", what, kindOf(v))
}

// expectList returns v as a sequence, accepting null, or fails naming what.
func expectList(v any, what string) ([]any, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case []any:
		return x, nil
	}
	return nil, errorf("%s: expected a list, got %s", what, kindOf(v))
}

func kindOf(v any) string {
	switch v.(type) {
	case *Mapping:
		return "a mapping"
	case []any:
		return "a list"
	}
	return fmt.Sprintf("%q", scalar(v))
}

func asList(v any) []string {
	switch x := v.(type) {
	case nil:
		return nil
	case []any:
		out := make([]string, len(x))
		for i, item := range x {
			out[i] = scalar(item)
		}
		return out
	}
	return []string{scalar(v)}
}
