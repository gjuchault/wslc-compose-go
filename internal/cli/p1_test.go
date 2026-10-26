package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"wslc-go/internal/compose"
)

// callWith reports whether a call contains every part.
func callWith(calls []string, parts ...string) bool {
	for _, c := range calls {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(c, p)
		}
		if ok {
			return true
		}
	}
	return false
}

const multiNetworkProject = `
name: demo
services:
  web:
    image: nginx
    networks:
      front: {aliases: [www]}
      back:
networks:
  front:
  back:
`

const multiNetworkListed = "{\"Name\":\"demo_front\"}\n{\"Name\":\"demo_back\"}\n"

func TestUpConnectsEveryNetwork(t *testing.T) {
	path := writeProject(t, multiNetworkProject)
	calls := useFake(t,
		rule{Prefix: "list"},
		rule{Prefix: "network list", Stdout: multiNetworkListed},
	)
	if code := Main([]string{"-f", path, "up", "-d"}); code != 0 {
		t.Errorf("exit code = %d", code)
	}
	got := calls()
	var order []string
	for _, c := range got {
		for _, prefix := range []string{"create", "network connect", "start", "run"} {
			if strings.HasPrefix(c, prefix) {
				order = append(order, prefix)
			}
		}
	}
	if want := []string{"create", "network connect", "start"}; !reflect.DeepEqual(order, want) {
		t.Errorf("calls = %q, want create, connect, start", got)
	}
	if !callWith(got, "create", "--network demo_front", "--network-alias www") ||
		!anyCall(got, "network connect --network-alias web demo_back demo-web-1") {
		t.Errorf("networks or aliases missing: %q", got)
	}
}

func TestUpRemovesContainerWhenConnectFails(t *testing.T) {
	path := writeProject(t, multiNetworkProject)
	calls := useFake(t,
		rule{Prefix: "list"},
		rule{Prefix: "network list", Stdout: multiNetworkListed},
		rule{Prefix: "network connect", Stderr: "boom", Exit: 1},
	)
	if code := Main([]string{"-f", path, "up", "-d"}); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if got := calls(); !anyCall(got, "remove -f demo-web-1") || anyCall(got, "start") {
		t.Errorf("a half-connected container must be removed, not started: %q", got)
	}
}

func TestConditionMet(t *testing.T) {
	healthcheck := map[string]any{"Healthcheck": map[string]any{"Test": []any{"CMD-SHELL", "true"}}}
	state := func(s map[string]any) map[string]any { return map[string]any{"State": s, "Config": healthcheck} }
	cases := []struct {
		name      string
		condition string
		data      map[string]any
		done      bool
		errPart   string
	}{
		{"healthy", compose.ConditionHealthy, state(map[string]any{"Running": true, "Health": map[string]any{"Status": "healthy"}}), true, ""},
		{"starting", compose.ConditionHealthy, state(map[string]any{"Running": true, "Health": map[string]any{"Status": "starting"}}), false, ""},
		{"not checked yet", compose.ConditionHealthy, state(map[string]any{"Running": true}), false, ""},
		{"unhealthy", compose.ConditionHealthy, state(map[string]any{"Running": true, "Health": map[string]any{"Status": "unhealthy"}}), false, "unhealthy"},
		{"exited", compose.ConditionHealthy, state(map[string]any{"Running": false, "Status": "exited", "ExitCode": 1.0, "Health": map[string]any{"Status": "starting"}}), false, "exited (1)"},
		{"no healthcheck", compose.ConditionHealthy, map[string]any{"State": map[string]any{"Running": true}}, false, "no healthcheck"},
		{"healthcheck NONE", compose.ConditionHealthy, map[string]any{"State": map[string]any{"Running": true},
			"Config": map[string]any{"Healthcheck": map[string]any{"Test": []any{"NONE"}}}}, false, "no healthcheck"},
		{"running", compose.ConditionCompleted, state(map[string]any{"Running": true}), false, ""},
		{"exit 0", compose.ConditionCompleted, state(map[string]any{"Running": false, "ExitCode": 0.0}), true, ""},
		{"exit 3", compose.ConditionCompleted, state(map[string]any{"Running": false, "ExitCode": 3.0}), false, "exit 3"},
	}
	for _, c := range cases {
		done, err := conditionMet("x", c.condition, c.data)
		if done != c.done || (err == nil) != (c.errPart == "") || (err != nil && !strings.Contains(err.Error(), c.errPart)) {
			t.Errorf("%s: done=%v err=%v", c.name, done, err)
		}
	}
}

func TestWaitForContainerPolls(t *testing.T) {
	statuses := []string{"starting", "starting", "healthy"}
	calls := 0
	oldInspect, oldInterval := inspectFn, pollInterval
	t.Cleanup(func() { inspectFn, pollInterval = oldInspect, oldInterval })
	pollInterval = time.Millisecond
	inspectFn = func(string) (map[string]any, error) {
		status := statuses[min(calls, len(statuses)-1)]
		calls++
		return map[string]any{"State": map[string]any{"Running": true, "Health": map[string]any{"Status": status}}}, nil
	}
	if err := waitForContainer("db", compose.ConditionHealthy); err != nil || calls != 3 {
		t.Errorf("err=%v calls=%d", err, calls)
	}
}

func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
}

func TestComposeFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"compose.yaml", "compose.override.yaml", "extra.yaml"} {
		os.WriteFile(filepath.Join(dir, name), []byte("services: {}\n"), 0o644)
	}
	sub := filepath.Join(dir, "sub")
	os.Mkdir(sub, 0o755)
	chdir(t, sub)
	t.Setenv("COMPOSE_FILE", "")

	files, err := (&globals{}).composeFiles()
	if err != nil || len(files) != 2 || filepath.Base(files[0]) != "compose.yaml" || filepath.Base(files[1]) != "compose.override.yaml" {
		t.Errorf("auto-detection: %q, %v", files, err)
	}

	files, err = (&globals{files: []string{"../extra.yaml"}}).composeFiles()
	if err != nil || !reflect.DeepEqual(files, []string{"../extra.yaml"}) {
		t.Errorf("-f disables the override file: %q, %v", files, err)
	}

	t.Setenv("COMPOSE_FILE", "../compose.yaml|../extra.yaml")
	t.Setenv("COMPOSE_PATH_SEPARATOR", "|")
	files, err = (&globals{}).composeFiles()
	if err != nil || !reflect.DeepEqual(files, []string{"../compose.yaml", "../extra.yaml"}) {
		t.Errorf("COMPOSE_FILE: %q, %v", files, err)
	}

	if _, err := (&globals{files: []string{"missing.yaml"}}).composeFiles(); err == nil {
		t.Error("a missing -f file is an error")
	}
}

func TestComposeFileFromDotenv(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".env"), []byte("COMPOSE_FILE=a.yaml\n"), 0o644)
	chdir(t, dir)
	t.Setenv("COMPOSE_FILE", "")
	os.Unsetenv("COMPOSE_FILE") // restored by t.Setenv's cleanup
	t.Setenv("COMPOSE_PATH_SEPARATOR", "")
	if got := composeFileVariable(""); !reflect.DeepEqual(got, []string{"a.yaml"}) {
		t.Errorf("got %q", got)
	}
}
