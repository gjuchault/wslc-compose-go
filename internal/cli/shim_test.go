package cli

import (
	"reflect"
	"testing"
)

func TestSplitArgv(t *testing.T) {
	cases := []struct {
		argv, compose, passthrough []string
	}{
		{[]string{"compose", "up", "-d"}, []string{"up", "-d"}, []string{}},
		{[]string{"compose"}, []string{}, []string{}},
		{[]string{"list", "-a"}, nil, []string{"list", "-a"}},
		{[]string{}, nil, []string{}},
	}
	for _, c := range cases {
		compose, passthrough := SplitArgv(c.argv)
		if !reflect.DeepEqual(compose, c.compose) || !reflect.DeepEqual(passthrough, c.passthrough) {
			t.Errorf("SplitArgv(%q) = %q, %q; want %q, %q", c.argv, compose, passthrough, c.compose, c.passthrough)
		}
	}
}
