package compose

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Compose-file variable interpolation: ${VAR}, ${VAR:-def}, ${VAR:?err}, $$ ...

var varRE = regexp.MustCompile(`\$(?:(\$)|([A-Za-z_][A-Za-z0-9_]*)|\{([A-Za-z_][A-Za-z0-9_]*)(?:(:?[-?+])([^}]*))?\})`)

// InterpolationError reports a ${VAR:?msg} whose variable is missing.
type InterpolationError struct{ Msg string }

func (e *InterpolationError) Error() string { return e.Msg }

func Interpolate(value string, env map[string]string) (string, error) {
	var out strings.Builder
	last := 0
	for _, m := range varRE.FindAllStringSubmatchIndex(value, -1) {
		out.WriteString(value[last:m[0]])
		last = m[1]
		group := func(i int) (string, bool) {
			if m[2*i] < 0 {
				return "", false
			}
			return value[m[2*i]:m[2*i+1]], true
		}

		if _, ok := group(1); ok {
			out.WriteString("$")
			continue
		}
		name, ok := group(2)
		if !ok {
			name, _ = group(3)
		}
		raw, set := env[name]
		if !set && caseInsensitiveEnv {
			raw, set = env[strings.ToUpper(name)]
		}
		sep, hasSep := group(4)
		if !hasSep {
			out.WriteString(raw)
			continue
		}
		operand, _ := group(5)
		// ":" variants treat an empty string like unset
		missing := !set
		if strings.HasPrefix(sep, ":") {
			missing = raw == ""
		}
		switch sep[len(sep)-1] {
		case '-':
			if missing {
				out.WriteString(operand)
			} else {
				out.WriteString(raw)
			}
		case '+':
			if !missing {
				out.WriteString(operand)
			}
		case '?':
			if missing {
				msg := operand
				if msg == "" {
					msg = fmt.Sprintf("required variable %s is missing", name)
				}
				return "", &InterpolationError{fmt.Sprintf("${%s}: %s", name, msg)}
			}
			out.WriteString(raw)
		}
	}
	out.WriteString(value[last:])
	return out.String(), nil
}

// InterpolateTree recursively interpolates every string in a parsed YAML tree.
func InterpolateTree(node any, env map[string]string) (any, error) {
	switch v := node.(type) {
	case string:
		return Interpolate(v, env)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			r, err := InterpolateTree(item, env)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	case *Mapping:
		out := &Mapping{Keys: slices.Clone(v.Keys), Values: make(map[string]any, len(v.Values))}
		for k, item := range v.Values {
			r, err := InterpolateTree(item, env)
			if err != nil {
				return nil, err
			}
			out.Values[k] = r
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			r, err := InterpolateTree(item, env)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	}
	return node, nil
}
