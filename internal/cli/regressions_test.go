package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"wslc-go/internal/compose"
	"wslc-go/internal/engine"
)

// The test binary doubles as a fake wslc (FAKE_WSLC=1). Each invocation is
// appended to FAKE_WSLC_LOG and answered by the first rule of
// FAKE_WSLC_RULES (JSON) whose prefix matches the joined arguments.
type rule struct {
	Prefix string
	Stdout string
	Stderr string
	Exit   int
	Sleep  float64
	// Grandchild leaves a descendant holding stdout for that many seconds
	Grandchild float64
}

func TestMain(m *testing.M) {
	if os.Getenv("FAKE_WSLC") == "1" {
		os.Exit(fakeWslc(os.Args[1:]))
	}
	exe, err := os.Executable()
	if err != nil {
		panic(err)
	}
	os.Setenv("WSLC_COMPOSE_BIN", exe)
	os.Setenv("FAKE_WSLC", "1")
	os.Exit(m.Run())
}

func fakeWslc(args []string) int {
	line := strings.Join(args, " ")
	if path := os.Getenv("FAKE_WSLC_LOG"); path != "" {
		if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintln(f, line)
			f.Close()
		}
	}
	var rules []rule
	json.Unmarshal([]byte(os.Getenv("FAKE_WSLC_RULES")), &rules)
	for _, r := range rules {
		if strings.HasPrefix(line, r.Prefix) {
			fmt.Fprint(os.Stdout, r.Stdout)
			fmt.Fprint(os.Stderr, r.Stderr)
			time.Sleep(time.Duration(r.Sleep * float64(time.Second)))
			if r.Grandchild > 0 {
				exe, _ := os.Executable()
				child := exec.Command(exe, "sleep")
				child.Env = append(os.Environ(), fmt.Sprintf(`FAKE_WSLC_RULES=[{"Prefix":"sleep","Sleep":%g}]`, r.Grandchild), "FAKE_WSLC_LOG=")
				child.Stdout = os.Stdout
				if child.Start() == nil && os.Getenv("FAKE_WSLC_PIDFILE") != "" {
					os.WriteFile(os.Getenv("FAKE_WSLC_PIDFILE"), []byte(strconv.Itoa(child.Process.Pid)), 0o644)
				}
			}
			return r.Exit
		}
	}
	return 0
}

// useFake installs rules for the fake wslc and returns a function listing the
// invocations made so far.
func useFake(t *testing.T, rules ...rule) func() []string {
	t.Helper()
	data, err := json.Marshal(rules)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_WSLC_RULES", string(data))
	log := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("FAKE_WSLC_LOG", log)
	return func() []string {
		data, _ := os.ReadFile(log)
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
}

func writeProject(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func anyCall(calls []string, prefix string) bool {
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

const webProject = "name: demo\nservices:\n  web:\n    image: nginx\n"

const webListed = `[{"Id":"abc","Name":"demo-web-1","Image":"nginx"}]`

func webInspect(t *testing.T, composePath string, running bool) string {
	t.Helper()
	project, err := compose.LoadProject(composePath, "", "")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal([]any{map[string]any{
		"Name": "demo-web-1",
		"Labels": map[string]string{
			compose.LabelProject:    "demo",
			compose.LabelService:    "web",
			compose.LabelIndex:      "1",
			compose.LabelConfigHash: project.Services["web"].ConfigHash(),
		},
		"State": map[string]any{"Running": running, "Status": "running"},
	}})
	return string(data)
}

// #4: a failed inspect must not be mistaken for a stale container.
func TestUpAbortsWhenInspectFails(t *testing.T) {
	path := writeProject(t, webProject)
	calls := useFake(t,
		rule{Prefix: "list", Stdout: webListed},
		rule{Prefix: "inspect", Stderr: "boom", Exit: 1},
		rule{Prefix: "network list", Stdout: "NETWORK ID  NAME\n"},
	)
	if code := Main([]string{"-f", path, "up", "-d"}); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if got := calls(); anyCall(got, "remove") || anyCall(got, "run") {
		t.Errorf("destructive calls after a failed inspect: %q", got)
	}
}

func TestUpLeavesUpToDateContainerAlone(t *testing.T) {
	path := writeProject(t, webProject)
	calls := useFake(t,
		rule{Prefix: "list", Stdout: webListed},
		rule{Prefix: "inspect", Stdout: webInspect(t, path, true)},
		rule{Prefix: "network list", Stdout: "NETWORK ID  NAME\nx  demo_default\n"},
	)
	if code := Main([]string{"-f", path, "up", "-d"}); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if got := calls(); anyCall(got, "remove") || anyCall(got, "run") || anyCall(got, "stop") {
		t.Errorf("unexpected calls: %q", got)
	}
}

// #9: a failing `wslc logs` must make `logs` fail.
func TestLogsFailsWhenWslcLogsFails(t *testing.T) {
	path := writeProject(t, webProject)
	useFake(t,
		rule{Prefix: "list", Stdout: webListed},
		rule{Prefix: "inspect", Stdout: webInspect(t, path, false)},
		rule{Prefix: "logs", Stderr: "ERROR_SHARING_VIOLATION\n", Exit: 1},
	)
	if code := Main([]string{"-f", path, "logs"}); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

// #11: dependencies in an inactive profile are an error, not silently dropped.
func TestSelectServicesWithProfiledDependency(t *testing.T) {
	path := writeProject(t, `
services:
  web:
    image: x
    depends_on: [db]
  db:
    image: y
    profiles: [debug]
`)
	project, err := compose.LoadProject(path, "", "")
	if err != nil {
		t.Fatal(err)
	}
	names := func(services []*compose.Service) []string {
		var out []string
		for _, s := range services {
			out = append(out, s.Name)
		}
		return out
	}

	for _, targets := range [][]string{nil, {"web"}} {
		if _, err := selectServices(project, targets, nil); err == nil || !strings.Contains(err.Error(), "disabled") {
			t.Errorf("selectServices(%q) err = %v, want disabled dependency error", targets, err)
		}
	}
	if got, err := selectServices(project, nil, []string{"debug"}); err != nil || strings.Join(names(got), ",") != "db,web" {
		t.Errorf("with profile: %q, %v", names(got), err)
	}
	if got, err := selectServices(project, []string{"db"}, nil); err != nil || strings.Join(names(got), ",") != "db" {
		t.Errorf("targeting db: %q, %v", names(got), err)
	}
}

// #5: a null device is not a terminal.
func TestIsTerminalFalseForNullDevice(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isTerminal(f) {
		t.Error("isTerminal(os.DevNull) = true")
	}
}

// #8: arguments after -- are positional.
func TestDoubleDashEndsOptions(t *testing.T) {
	path := writeProject(t, "name: demo\nservices:\n  web:\n    image: nginx\n    build: .\n")
	calls := useFake(t, rule{Prefix: "network list", Stdout: "NETWORK ID  NAME\n"})
	if code := Main([]string{"-f", path, "up", "-d", "--", "--build"}); code != 1 {
		t.Errorf("exit code = %d, want 1 (no such service: --build)", code)
	}
	if got := calls(); anyCall(got, "build") || anyCall(got, "run") {
		t.Errorf("--build after -- was treated as an option: %q", got)
	}
}

// #14: argparse-style attached values, grouped short flags and abbreviations.
func TestArgparseStyleOptions(t *testing.T) {
	path := writeProject(t, webProject)
	rules := []rule{
		{Prefix: "list", Stdout: webListed},
		{Prefix: "inspect", Stdout: webInspect(t, path, true)},
	}

	calls := useFake(t, rules...)
	if code := Main([]string{"-f", path, "exec", "-T", "-uroot", "-eK=V", "web", "ls", "-la"}); code != 0 {
		t.Errorf("exec exit code = %d", code)
	}
	if got := calls(); !anyCall(got, "exec -u root -e K=V demo-web-1 ls -la") {
		t.Errorf("exec calls = %q", got)
	}

	calls = useFake(t, rules...)
	if code := Main([]string{"-f", path, "logs", "-ft", "-n5", "web"}); code != 0 {
		t.Errorf("logs exit code = %d", code)
	}
	if got := calls(); !anyCall(got, "logs -f -n 5 -t demo-web-1") {
		t.Errorf("logs calls = %q", got)
	}

	calls = useFake(t, rules...)
	if code := Main([]string{"-f", path, "stop", "--time", "3"}); code != 0 {
		t.Errorf("stop exit code = %d", code)
	}
	if got := calls(); !anyCall(got, "stop -t 3 demo-web-1") {
		t.Errorf("stop calls = %q", got)
	}

	useFake(t, rules...)
	if code := Main([]string{"-f", path, "logs", "--t"}); code != 2 {
		t.Errorf("ambiguous --t: exit code = %d, want 2", code)
	}
}

// #6: followers already started are reaped when a later one fails to start.
func TestFollowLogsCleansUpWhenAStartFails(t *testing.T) {
	useFake(t, rule{Prefix: "logs", Sleep: 60})
	var started []*exec.Cmd
	orig := newCommand
	t.Cleanup(func() { newCommand = orig })
	newCommand = func(args []string) (*exec.Cmd, error) {
		if len(started) == 1 {
			return exec.Command(filepath.Join(t.TempDir(), "missing-wslc")), nil
		}
		cmd, err := engine.Command(args)
		started = append(started, cmd)
		return cmd, err
	}

	containers := []container{{Name: "demo-a-1", Running: true}, {Name: "demo-b-1", Running: true}}
	begin := time.Now()
	if _, err := followContainers(containers, logOptions{follow: true}, nil); err == nil {
		t.Error("expected an error when a follower cannot start")
	}
	if started[0].ProcessState == nil {
		t.Error("first follower was not reaped")
	}
	if time.Since(begin) > 30*time.Second {
		t.Error("cleanup waited for the follower instead of stopping it")
	}
}

// #6/#7: an interrupt stops and reaps every follower, and is not a failure.
func TestFollowLogsStopsOnInterrupt(t *testing.T) {
	useFake(t, rule{Prefix: "logs", Sleep: 60})
	var started []*exec.Cmd
	orig := newCommand
	t.Cleanup(func() { newCommand = orig })
	newCommand = func(args []string) (*exec.Cmd, error) {
		cmd, err := engine.Command(args)
		started = append(started, cmd)
		return cmd, err
	}

	interrupted := make(chan struct{})
	time.AfterFunc(300*time.Millisecond, func() { close(interrupted) })
	containers := []container{{Name: "demo-a-1", Running: true}, {Name: "demo-b-1", Running: true}}
	begin := time.Now()
	code, err := followContainers(containers, logOptions{follow: true}, interrupted)
	if code != 0 || err != nil {
		t.Errorf("code=%d err=%v, want 0 <nil>", code, err)
	}
	for i, cmd := range started {
		if cmd.ProcessState == nil {
			t.Errorf("follower %d was not reaped", i)
		}
	}
	if time.Since(begin) > 30*time.Second {
		t.Error("followers were not stopped on interrupt")
	}
}

// Regression tests for the issues found in the second Opus/Astra review.

func TestOptionValueThatLooksLikeAnOptionIsRejected(t *testing.T) {
	path := writeProject(t, webProject)
	calls := useFake(t, rule{Prefix: "network list", Stdout: "NETWORK ID  NAME\n"})
	if code := Main([]string{"-f", path, "--profile", "--dry-run", "up", "-d"}); code != 2 {
		t.Errorf("exit code = %d, want 2 (--profile expects one argument)", code)
	}
	if got := calls(); len(got) > 1 || got[0] != "" {
		t.Errorf("wslc was called: %q", got)
	}

	fs := newFlagSet("test", "")
	tail := fs.decimalIntP("tail", "n", 0)
	if _, err := fs.parse([]string{"-n", "-1"}); err != nil || *tail != -1 {
		t.Errorf("-n -1: tail=%d err=%v, want -1 (negative numbers are values)", *tail, err)
	}
}

func TestIntegerOptionsAreDecimal(t *testing.T) {
	for arg, want := range map[string]int{"010": 10, "08": 8, "+5": 5} {
		fs := newFlagSet("test", "")
		index := fs.decimalIntP("index", "", 1)
		if _, err := fs.parse([]string{"--index", arg}); err != nil || *index != want {
			t.Errorf("--index %s: got %d, %v; want %d", arg, *index, err, want)
		}
	}
	fs := newFlagSet("test", "")
	fs.decimalIntP("index", "", 1)
	if _, err := fs.parse([]string{"--index", "0x10"}); err == nil {
		t.Error("--index 0x10: expected an error")
	}
}

func TestHelpAbbreviation(t *testing.T) {
	if code := Main([]string{"logs", "--he"}); code != 0 {
		t.Errorf("logs --he: exit code = %d, want 0", code)
	}
}

func TestDownRemovesContainersThatCannotBeInspected(t *testing.T) {
	path := writeProject(t, webProject)
	calls := useFake(t,
		rule{Prefix: "list", Stdout: webListed},
		rule{Prefix: "inspect", Stderr: "boom", Exit: 1},
		rule{Prefix: "network list", Stdout: "NETWORK ID  NAME\n"},
	)
	if code := Main([]string{"-f", path, "down"}); code != 0 {
		t.Errorf("down exit code = %d, want 0", code)
	}
	if got := calls(); !anyCall(got, "remove -f demo-web-1") {
		t.Errorf("down calls = %q", got)
	}
}

// A follower failing after an interrupt is not a failure. The race where the
// followers die of the Ctrl+C before the interrupt is recorded (both select
// cases ready) cannot be reproduced deterministically: this covers the rest.
func TestFollowLogsInterruptIsNotAFailure(t *testing.T) {
	useFake(t, rule{Prefix: "logs", Exit: 1})
	interrupted := make(chan struct{})
	close(interrupted)
	containers := []container{{Name: "demo-a-1", Running: true}}
	if code, err := followContainers(containers, logOptions{follow: true}, interrupted); code != 0 || err != nil {
		t.Errorf("code=%d err=%v, want 0 <nil>", code, err)
	}
}

func TestFollowLogsReturnsWhenADescendantHoldsTheOutput(t *testing.T) {
	useFake(t, rule{Prefix: "logs", Stdout: "partial line", Grandchild: 15})
	killDescendantAfter(t)
	containers := []container{{Name: "demo-a-1", Running: false}}
	begin := time.Now()
	if code, err := followContainers(containers, logOptions{}, nil); code != 0 || err != nil {
		t.Errorf("code=%d err=%v, want 0 <nil>", code, err)
	}
	if elapsed := time.Since(begin); elapsed > 10*time.Second {
		t.Errorf("logs took %v, waiting for the descendant's output", elapsed)
	}
}

// killDescendantAfter has the fake record the PID of the descendant it leaves
// behind, and kills it when the test ends (it would lock the test binary).
func killDescendantAfter(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	t.Setenv("FAKE_WSLC_PIDFILE", pidFile)
	t.Cleanup(func() {
		data, _ := os.ReadFile(pidFile)
		if pid, err := strconv.Atoi(string(data)); err == nil {
			if p, err := os.FindProcess(pid); err == nil {
				p.Kill()
				p.Wait()
			}
		}
	})
}

// Regression tests for the issues found in the third Opus/Astra review.

func TestStopFailsWhenAContainerCannotBeInspected(t *testing.T) {
	path := writeProject(t, webProject)
	for _, action := range []string{"stop", "restart"} {
		calls := useFake(t,
			rule{Prefix: "list", Stdout: webListed},
			rule{Prefix: "inspect", Stderr: "boom", Exit: 1},
		)
		if code := Main([]string{"-f", path, action}); code != 1 {
			t.Errorf("%s: exit code = %d, want 1", action, code)
		}
		if got := calls(); anyCall(got, "start") {
			t.Errorf("%s started a container in an unknown state: %q", action, got)
		}
	}
}

func TestDownFallsBackToTheContainerID(t *testing.T) {
	path := writeProject(t, webProject)
	calls := useFake(t,
		rule{Prefix: "list", Stdout: `[{"Id":"abc","Image":"nginx"}]`},
		rule{Prefix: "inspect", Exit: 1},
		rule{Prefix: "network list", Stdout: "NETWORK ID  NAME\n"},
	)
	if code := Main([]string{"-f", path, "down"}); code != 0 {
		t.Errorf("down exit code = %d, want 0", code)
	}
	if got := calls(); !anyCall(got, "remove -f abc") {
		t.Errorf("down calls = %q", got)
	}
}

func TestOptionValueWithASpaceIsAValue(t *testing.T) {
	fs := newFlagSet("test", "")
	env := fs.StringArrayP("env", "e", nil, "")
	if _, err := fs.parse([]string{"-e", "-X foo", "--env", "-Y bar"}); err != nil || len(*env) != 2 {
		t.Errorf("env=%q err=%v, want both values", *env, err)
	}
}

func TestPrefixWriterSplitsLines(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	var mu sync.Mutex
	out := &prefixWriter{prefix: "a | ", mu: &mu}
	for _, chunk := range []string{"one\ntw", "o", "\nthree\nfo", "ur"} {
		out.Write([]byte(chunk))
	}
	out.flush()
	os.Stdout = stdout
	w.Close()
	data, _ := io.ReadAll(r)
	if want := "a | one\na | two\na | three\na | four\n"; string(data) != want {
		t.Errorf("output = %q, want %q", data, want)
	}
}

// Regression tests for the issues found in the fourth Opus/Astra review.

func TestStopIgnoresUninspectableContainersOfOtherServices(t *testing.T) {
	path := writeProject(t, "name: demo\nservices:\n  web:\n    image: nginx\n  db:\n    image: postgres\n")
	calls := useFake(t,
		rule{Prefix: "list", Stdout: `[{"Id":"abc","Name":"demo-web-1"},{"Id":"def","Name":"demo-db-1"}]`},
		rule{Prefix: "inspect abc", Stdout: webInspect(t, path, true)},
		rule{Prefix: "inspect def", Stderr: "boom", Exit: 1},
	)
	if code := Main([]string{"-f", path, "stop", "web"}); code != 0 {
		t.Errorf("stop web: exit code = %d, want 0", code)
	}
	if got := calls(); !anyCall(got, "stop -t 10 demo-web-1") {
		t.Errorf("stop calls = %q", got)
	}

	useFake(t,
		rule{Prefix: "list", Stdout: `[{"Id":"abc","Name":"demo-web-1"},{"Id":"def","Name":"demo-db-1"}]`},
		rule{Prefix: "inspect abc", Stdout: webInspect(t, path, true)},
		rule{Prefix: "inspect def", Stderr: "boom", Exit: 1},
	)
	if code := Main([]string{"-f", path, "stop", "db"}); code != 1 {
		t.Errorf("stop db: exit code = %d, want 1 (its own container cannot be inspected)", code)
	}
}

func TestPositionalStartingWithADashAndASpace(t *testing.T) {
	fs := newFlagSet("test", "")
	follow := fs.BoolP("follow", "f", false, "")
	names, err := fs.parse([]string{"-x y", "-f", "web"})
	if err != nil || !*follow || len(names) != 2 || names[0] != "-x y" || names[1] != "web" {
		t.Errorf("names=%q follow=%v err=%v", names, *follow, err)
	}

	fs = newFlagSet("test", "")
	fs.BoolP("tty", "T", false, "")
	fs.stopAtFirstArg()
	if names, err := fs.parse([]string{"-T", "-a b", "-T"}); err != nil || len(names) != 2 || names[0] != "-a b" || names[1] != "-T" {
		t.Errorf("stopAtFirstArg: names=%q err=%v", names, err)
	}
}
