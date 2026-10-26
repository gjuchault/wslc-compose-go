package compose

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

var testEnv = map[string]string{"NAME": "world", "EMPTY": ""}

func TestInterpolate(t *testing.T) {
	cases := []struct{ in, want string }{
		// simple
		{"hello $NAME", "hello world"},
		{"hello ${NAME}", "hello world"},
		// missing defaults to empty
		{"x${MISSING}y", "xy"},
		// default operator
		{"${MISSING:-def}", "def"},
		{"${EMPTY:-def}", "def"},
		{"${EMPTY-def}", ""},
		{"${NAME:-def}", "world"},
		// alt operator
		{"${NAME:+set}", "set"},
		{"${EMPTY:+set}", ""},
		{"${EMPTY+set}", "set"},
		// required operator
		{"${NAME:?boom}", "world"},
		// escape
		{"cost: $$5 and $$(cmd)", "cost: $5 and $(cmd)"},
		// shell command not interpolated
		{"$(date)", "$(date)"},
	}
	for _, c := range cases {
		got, err := Interpolate(c.in, testEnv)
		if err != nil || got != c.want {
			t.Errorf("Interpolate(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

func TestInterpolateRequired(t *testing.T) {
	var ie *InterpolationError
	if _, err := Interpolate("${MISSING:?boom}", testEnv); !errors.As(err, &ie) || !strings.Contains(err.Error(), "boom") {
		t.Errorf("expected InterpolationError mentioning boom, got %v", err)
	}
	if _, err := Interpolate("${EMPTY:?}", testEnv); !errors.As(err, &ie) {
		t.Errorf("expected InterpolationError, got %v", err)
	}
}

func TestInterpolateTree(t *testing.T) {
	tree := map[string]any{"a": []any{"$NAME", 1, nil}, "b": map[string]any{"c": "${MISSING:-x}"}}
	got, err := InterpolateTree(tree, testEnv)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"a": []any{"world", 1, nil}, "b": map[string]any{"c": "x"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

func TestShlexSplit(t *testing.T) {
	cases := map[string][]string{
		`sh -c 'sleep 999'`:         {"sh", "-c", "sleep 999"},
		`echo "a \"b\" \c" d\ e ''`: {"echo", `a "b" \c`, "d e", ""},
		"  spaced\targs  ":          {"spaced", "args"},
	}
	for in, want := range cases {
		got, err := shlexSplit(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("shlexSplit(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := shlexSplit(`echo 'oops`); err == nil {
		t.Error("expected error for unclosed quote")
	}
}
