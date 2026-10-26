package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"wslc-go/internal/compose"
)

// inspectJSON renders wslc inspect output for a container of the demo project.
func inspectJSON(name, service string, extra map[string]string, state map[string]any) string {
	labels := map[string]string{compose.LabelProject: "demo", compose.LabelService: service}
	for k, v := range extra {
		labels[k] = v
	}
	data, _ := json.Marshal([]any{map[string]any{"Name": "/" + name, "Labels": labels, "State": state}})
	return string(data)
}

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

// The list in wslc 3's format: one object per line, docker's keys.
const orphanListed = `{"ID":"abc","Names":"demo-web-1","Image":"nginx","Ports":""}
{"ID":"def","Names":"demo-old-1","Image":"nginx","Ports":""}
{"ID":"ghi","Names":"demo-web-run-1","Image":"nginx","Ports":""}
`

func orphanRules(t *testing.T, path string) []rule {
	running := map[string]any{"Running": true, "Status": "running"}
	return []rule{
		{Prefix: "list", Stdout: orphanListed},
		{Prefix: "inspect abc", Stdout: webInspect(t, path, true)},
		{Prefix: "inspect def", Stdout: inspectJSON("demo-old-1", "old", nil, running)},
		{Prefix: "inspect ghi", Stdout: inspectJSON("demo-web-run-1", "web", map[string]string{compose.LabelOneOff: "True"}, running)},
		{Prefix: "network list", Stdout: `{"Name":"demo_default"}` + "\n"},
	}
}

func TestUpWarnsAboutOrphans(t *testing.T) {
	path := writeProject(t, webProject)
	t.Setenv("COMPOSE_REMOVE_ORPHANS", "")
	calls := useFake(t, orphanRules(t, path)...)
	if code := Main([]string{"-f", path, "up", "-d"}); code != 0 {
		t.Errorf("exit code = %d", code)
	}
	if got := calls(); anyCall(got, "remove") || anyCall(got, "stop") || anyCall(got, "run") || anyCall(got, "network create") {
		t.Errorf("up without --remove-orphans changed something: %q", got)
	}
}

func TestUpRemovesOrphans(t *testing.T) {
	path := writeProject(t, webProject)
	for _, viaEnv := range []bool{false, true} {
		args := []string{"-f", path, "up", "-d"}
		if viaEnv {
			t.Setenv("COMPOSE_REMOVE_ORPHANS", "1")
		} else {
			t.Setenv("COMPOSE_REMOVE_ORPHANS", "")
			args = append(args, "--remove-orphans")
		}
		calls := useFake(t, orphanRules(t, path)...)
		if code := Main(args); code != 0 {
			t.Errorf("exit code = %d", code)
		}
		got := calls()
		if !anyCall(got, "stop -t 10 demo-old-1") || !anyCall(got, "remove -f demo-old-1") {
			t.Errorf("viaEnv=%v: orphan not removed: %q", viaEnv, got)
		}
		if anyCall(got, "remove -f demo-web") {
			t.Errorf("viaEnv=%v: service or one-off container removed: %q", viaEnv, got)
		}
	}
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

const runProject = `
name: demo
services:
  web:
    image: nginx
    command: nginx
    ports: ["8080:80"]
    environment: {A: "1"}
    depends_on:
      db: {condition: service_healthy}
  db:
    image: postgres
`

func TestRunOneOffContainer(t *testing.T) {
	path := writeProject(t, runProject)
	calls := useFake(t, rule{Prefix: "network list", Stdout: `{"Name":"demo_default"}` + "\n"})
	code := Main([]string{"-f", path, "run", "--rm", "-T", "--no-deps", "-e", "B=2", "-p", "9000:9000", "web", "echo", "hi"})
	if code != 0 {
		t.Errorf("exit code = %d", code)
	}
	got := calls()
	if anyCall(got, "list") || anyCall(got, "inspect") {
		t.Errorf("--no-deps looked at the dependencies: %q", got)
	}
	if !callWith(got, "run --rm --name demo-web-run-", "-l "+compose.LabelOneOff+"=True", "-e A=1", "-e B=2", "-p 9000:9000", " -i ", "nginx echo hi") {
		t.Errorf("run call = %q", got)
	}
	if callWith(got, "run", "8080:80") || callWith(got, "run", "--network-alias") || callWith(got, "run", " -t ") {
		t.Errorf("service ports, aliases or a TTY leaked into the one-off container: %q", got)
	}
}

func TestRunStartsDependenciesAndWaits(t *testing.T) {
	path := writeProject(t, runProject)
	healthy := inspectJSON("demo-db-1", "db", nil, map[string]any{"Running": true, "Status": "running", "Health": map[string]any{"Status": "healthy"}})
	calls := useFake(t,
		rule{Prefix: "list"},
		rule{Prefix: "inspect demo-db-1", Stdout: healthy},
		rule{Prefix: "network list", Stdout: `{"Name":"demo_default"}` + "\n"},
	)
	if code := Main([]string{"-f", path, "run", "-d", "--name", "job", "-P", "web"}); code != 0 {
		t.Errorf("exit code = %d", code)
	}
	got := calls()
	if !callWith(got, "run -d --name demo-db-1") || !anyCall(got, "inspect demo-db-1") {
		t.Errorf("dependency not started or not waited for: %q", got)
	}
	if !callWith(got, "run -d --name job", "-p 8080:80", "nginx nginx") {
		t.Errorf("one-off call = %q", got)
	}
	if callWith(got, "run", "--name demo-web-1") {
		t.Errorf("run started the service itself: %q", got)
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
