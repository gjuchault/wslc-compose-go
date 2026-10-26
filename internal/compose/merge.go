package compose

import (
	"fmt"
	"strings"
)

// Merging of several compose files (-f a.yaml -f b.yaml, compose.override.yaml),
// following https://github.com/compose-spec/compose-spec/blob/main/13-merge.md:
// mappings merge recursively, sequences append, and the override wins for
// scalars. Within a service, command, entrypoint and healthcheck.test are
// replaced, ports and volumes are unique by their published port and target,
// and attributes with a list and a mapping syntax (environment, labels,
// depends_on, networks, ...) are merged as mappings.

// resetTag is a value tagged !reset: the attribute is removed.
type resetTag struct{}

// overrideTag is a value tagged !override: it replaces the earlier value
// instead of being merged with it.
type overrideTag struct{ value any }

// mergeFiles merges parsed compose files in order and resolves the !reset and
// !override tags, even with a single file.
func mergeFiles(trees []any) any {
	var result any
	for _, tree := range trees {
		if result == nil {
			result = resolveTags(tree)
			continue
		}
		result = mergeTop(result.(*Mapping), tree.(*Mapping))
	}
	return result
}

// resolveTags drops !reset entries and unwraps !override values.
func resolveTags(v any) any {
	switch x := v.(type) {
	case overrideTag:
		return resolveTags(x.value)
	case *Mapping:
		out := newMapping()
		for _, k := range x.Keys {
			if _, reset := x.Values[k].(resetTag); !reset {
				out.set(k, resolveTags(x.Values[k]))
			}
		}
		return out
	case []any:
		out := make([]any, 0, len(x))
		for _, item := range x {
			if _, reset := item.(resetTag); !reset {
				out = append(out, resolveTags(item))
			}
		}
		return out
	}
	return v
}

func mergeTop(base, override *Mapping) *Mapping {
	return mergeMapping(base, override, func(key string, b, o any) any {
		if key != "services" {
			return mergeValue(b, o)
		}
		baseServices, ok1 := b.(*Mapping)
		overrideServices, ok2 := o.(*Mapping)
		if !ok1 || !ok2 {
			return mergeValue(b, o)
		}
		return mergeMapping(baseServices, overrideServices, func(_ string, b, o any) any {
			bs, ok1 := b.(*Mapping)
			ov, ok2 := o.(*Mapping)
			if !ok1 || !ok2 {
				return mergeValue(b, o)
			}
			return mergeService(bs, ov)
		})
	})
}

// mergeMapping merges override into base: keys keep their first position,
// a !reset value removes the key, and merge decides the value of keys present
// in both.
func mergeMapping(base, override *Mapping, merge func(key string, b, o any) any) *Mapping {
	out := newMapping()
	for _, k := range base.Keys {
		out.set(k, base.Values[k])
	}
	for _, k := range override.Keys {
		o := override.Values[k]
		switch {
		case isReset(o):
			out.remove(k)
		case isOverride(o) || !out.Has(k):
			out.set(k, resolveTags(o))
		default:
			out.set(k, merge(k, out.Values[k], o))
		}
	}
	return out
}

func isReset(v any) bool    { _, ok := v.(resetTag); return ok }
func isOverride(v any) bool { _, ok := v.(overrideTag); return ok }

// mergeValue is the generic rule: mappings merge, sequences append (scalars
// only once), anything else is replaced.
func mergeValue(base, override any) any {
	switch o := override.(type) {
	case *Mapping:
		if b, ok := base.(*Mapping); ok {
			return mergeMapping(b, o, func(_ string, b, o any) any { return mergeValue(b, o) })
		}
	case []any:
		if b, ok := base.([]any); ok {
			return appendUnique(b, o, func(v any) (string, bool) {
				switch v.(type) {
				case *Mapping, []any:
					return "", false
				}
				return scalar(v), true
			})
		}
	}
	return resolveTags(override)
}

// appendUnique appends the items of override to base; an item whose key
// matches an earlier one replaces it in place.
func appendUnique(base, override []any, key func(any) (string, bool)) []any {
	out := append([]any(nil), base...)
	for _, item := range override {
		if isReset(item) {
			continue
		}
		item = resolveTags(item)
		replaced := false
		if k, ok := key(item); ok {
			for i, existing := range out {
				if ek, ok := key(existing); ok && ek == k {
					out[i], replaced = item, true
					break
				}
			}
		}
		if !replaced {
			out = append(out, item)
		}
	}
	return out
}

// mappingSyntaxKeys accept "KEY=VALUE" lists as well as mappings.
var mappingSyntaxKeys = []string{"environment", "labels", "annotations", "sysctls"}

// listSyntaxKeys accept a single string as well as a list.
var listSyntaxKeys = []string{"env_file", "dns", "dns_search", "dns_opt", "tmpfs", "profiles"}

func mergeService(base, override *Mapping) *Mapping {
	base, override = normalizeService(base), normalizeService(override)
	return mergeMapping(base, override, func(key string, b, o any) any {
		switch key {
		case "command", "entrypoint":
			return resolveTags(o)
		case "ports":
			return mergeSequence(b, o, portKey)
		case "volumes":
			return mergeSequence(b, o, volumeKey)
		case "healthcheck":
			bh, ok1 := b.(*Mapping)
			oh, ok2 := o.(*Mapping)
			if !ok1 || !ok2 {
				return resolveTags(o)
			}
			return mergeMapping(bh, oh, func(key string, b, o any) any {
				if key == "test" {
					return resolveTags(o)
				}
				return mergeValue(b, o)
			})
		}
		return mergeValue(b, o)
	})
}

func mergeSequence(b, o any, key func(any) (string, bool)) any {
	bl, ok1 := b.([]any)
	ol, ok2 := o.([]any)
	if !ok1 || !ok2 {
		return resolveTags(o)
	}
	return appendUnique(bl, ol, key)
}

// normalizeService rewrites the attributes with alternative syntaxes into one
// form, so that a list in one file merges with a mapping in another.
func normalizeService(svc *Mapping) *Mapping {
	out := newMapping()
	for _, k := range svc.Keys {
		out.set(k, svc.Values[k])
	}
	for _, key := range mappingSyntaxKeys {
		if list, ok := out.Get(key).([]any); ok {
			out.Values[key] = keyValueMapping(list)
		}
	}
	for _, key := range listSyntaxKeys {
		if v := out.Get(key); v != nil && !isTagged(v) {
			if _, ok := v.([]any); !ok {
				out.Values[key] = []any{v}
			}
		}
	}
	for _, key := range []string{"depends_on", "networks"} {
		if list, ok := out.Get(key).([]any); ok {
			m := newMapping()
			for _, item := range list {
				m.set(scalar(item), nil)
			}
			out.Values[key] = m
		}
	}
	switch build := out.Get("build").(type) {
	case string:
		m := newMapping()
		m.set("context", build)
		out.Values["build"] = m
	case *Mapping:
		if list, ok := build.Get("args").([]any); ok {
			m := newMapping()
			for _, k := range build.Keys {
				m.set(k, build.Values[k])
			}
			m.Values["args"] = keyValueMapping(list)
			out.Values["build"] = m
		}
	}
	return out
}

func isTagged(v any) bool { return isReset(v) || isOverride(v) }

// keyValueMapping turns ["A=1", "B"] into {A: "1", B: null}.
func keyValueMapping(list []any) *Mapping {
	m := newMapping()
	for _, item := range list {
		if isTagged(item) {
			continue
		}
		key, value, found := strings.Cut(scalar(item), "=")
		if found {
			m.set(key, value)
		} else {
			m.set(key, nil)
		}
	}
	return m
}

// portKey identifies a port by host IP, published port, target and protocol.
func portKey(v any) (string, bool) {
	mappings, err := ParsePort(v)
	if err != nil || len(mappings) != 1 {
		if _, ok := v.(*Mapping); ok {
			return "", false
		}
		return scalar(v), true // ranges and uninterpolated values: by text
	}
	p := mappings[0]
	return fmt.Sprintf("%s|%d|%s", p.Published, p.Target, p.Protocol), true
}

// volumeKey identifies a mount by its target in the container.
func volumeKey(v any) (string, bool) {
	if m, ok := v.(*Mapping); ok {
		return scalar(m.Get("target")), true
	}
	parts := splitVolumeSpec(scalar(v))
	if len(parts) == 1 {
		return parts[0], true
	}
	return parts[1], true
}
