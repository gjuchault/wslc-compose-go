// Package engine is a thin subprocess layer over the wslc CLI.
package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"wslc-go/internal/compose"
)

var winPathRE = regexp.MustCompile(`^([A-Za-z]:[\\/]|\\\\)`)

var wslcFallbacks = []string{
	"/mnt/c/Program Files/WSL/wslc.exe",
	`C:\Program Files\WSL\wslc.exe`,
}

// Error is a failed or unavailable wslc invocation.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

// ErrInterrupted is returned when Ctrl+C was pressed while a wslc command ran
// in the foreground.
var ErrInterrupted = errors.New("interrupted")

// isSelf reports whether path is our own `wslc` wrapper, not the real CLI.
func isSelf(path string) bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	a, err1 := filepath.EvalSymlinks(path)
	b, err2 := filepath.EvalSymlinks(self)
	return err1 == nil && err2 == nil && strings.EqualFold(a, b)
}

var findWslc = sync.OnceValues(func() (string, error) {
	if override := os.Getenv("WSLC_COMPOSE_BIN"); override != "" {
		return override, nil
	}
	for _, name := range []string{"wslc.exe", "wslc"} {
		if path, err := exec.LookPath(name); err == nil && !isSelf(path) {
			return path, nil
		}
	}
	for _, path := range wslcFallbacks {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, nil
		}
	}
	return "", &Error{"wslc CLI not found. Install the WSL container preview " +
		"(https://learn.microsoft.com/windows/wsl/wsl-container) or set WSLC_COMPOSE_BIN."}
})

// FindWslc locates the real wslc executable (cached).
func FindWslc() (string, error) { return findWslc() }

func runningInWSL() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if _, ok := os.LookupEnv("WSL_DISTRO_NAME"); ok {
		return true
	}
	release, _ := os.ReadFile("/proc/sys/kernel/osrelease")
	return strings.Contains(strings.ToLower(string(release)), "microsoft")
}

// needsPathTranslation is true when we call the Windows wslc.exe from inside a WSL distro.
var needsPathTranslation = sync.OnceValue(func() bool {
	wslc, err := FindWslc()
	return err == nil && runningInWSL() && strings.HasSuffix(strings.ToLower(wslc), ".exe")
})

var (
	hostPathMu    sync.Mutex
	hostPathCache = map[string]string{}
	mntPathRE     = regexp.MustCompile(`^/mnt/([a-zA-Z])(/.*)?$`)
)

// ToHostPath translates a Linux path to the Windows path wslc.exe expects.
func ToHostPath(path string) string {
	if !needsPathTranslation() || winPathRE.MatchString(path) {
		return path
	}
	hostPathMu.Lock()
	defer hostPathMu.Unlock()
	if cached, ok := hostPathCache[path]; ok {
		return cached
	}
	result := translatePath(path)
	hostPathCache[path] = result
	return result
}

func translatePath(path string) string {
	if out, err := exec.Command("wslpath", "-w", path).Output(); err == nil {
		if s := strings.TrimSpace(string(out)); s != "" {
			return s
		}
	}
	// manual fallback for /mnt/<drive>/... paths
	if m := mntPathRE.FindStringSubmatch(path); m != nil {
		rest := m[2]
		if rest == "" {
			rest = "/"
		}
		return strings.ToUpper(m[1]) + ":" + strings.ReplaceAll(rest, "/", `\`)
	}
	fmt.Fprintf(os.Stderr, "wslc-compose: warning: cannot translate path %q for wslc.exe; passing as-is\n", path)
	return path
}

// Opts tunes a single wslc invocation.
type Opts struct {
	Capture bool // collect stdout/stderr instead of streaming them
	NoCheck bool // do not turn a non-zero exit code into an error
	DryRun  bool // print the command instead of running it
}

type Result struct {
	Code   int
	Stdout string
	Stderr string
}

func head(args []string, n int) string {
	if len(args) < n {
		n = len(args)
	}
	return strings.Join(args[:n], " ")
}

// Run invokes wslc with args. Without Capture the child is attached to the
// terminal. Once interrupted (see HandleInterrupts), Run stops the child
// (see wait) and reports ErrInterrupted.
func Run(args []string, o Opts) (*Result, error) {
	wslc, err := FindWslc()
	if err != nil {
		return nil, err
	}
	if o.DryRun {
		fmt.Println("+ " + strings.Join(append([]string{wslc}, args...), " "))
		return &Result{}, nil
	}
	select {
	case <-Interrupted():
		return nil, ErrInterrupted
	default:
	}

	cmd := command(wslc, args)
	var stdout, stderr bytes.Buffer
	if o.Capture {
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
	} else {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	}
	if err := cmd.Start(); err != nil {
		return nil, &Error{fmt.Sprintf("cannot run %s: %v", wslc, err)}
	}
	err, interrupted := wait(cmd)

	res := &Result{Stdout: stdout.String(), Stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case interrupted:
		return res, ErrInterrupted
	case errors.As(err, &exitErr):
		res.Code = exitErr.ExitCode()
	case errors.Is(err, exec.ErrWaitDelay): // exited 0, a descendant kept the output open
	case err != nil:
		return nil, &Error{fmt.Sprintf("cannot run %s: %v", wslc, err)}
	}
	if !o.NoCheck && res.Code != 0 {
		msg := fmt.Sprintf("wslc %s failed (exit %d)", head(args, 2), res.Code)
		if detail := strings.TrimSpace(res.Stderr); detail != "" {
			msg += ": " + detail
		}
		return res, &Error{msg}
	}
	return res, nil
}

// RunInteractive runs wslc attached to the terminal and returns its exit code.
func RunInteractive(args []string, dryRun bool) (int, error) {
	res, err := Run(args, Opts{NoCheck: true, DryRun: dryRun})
	if errors.Is(err, ErrInterrupted) {
		return 130, err
	}
	if err != nil {
		return 1, err
	}
	return res.Code, nil
}

// Windows-side errors wslc can emit transiently: right after a stop the
// container's kernel object is not always released yet (ERROR_ALREADY_EXISTS
// on the next start), and the preview's session store rejects overlapping
// invocations (ERROR_SHARING_VIOLATION). A bounded retry rides out the race;
// a persistent machine-wide lock (docs/MIGRATION.md) still fails after the
// last attempt.
var transientErrorMarkers = []string{"ERROR_ALREADY_EXISTS", "ERROR_SHARING_VIOLATION"}

const (
	transientRetries = 5
	transientDelay   = 2 * time.Second
)

// overridable in tests
var (
	runFn   = Run
	sleepFn = time.Sleep
)

// RunRetried is Run with Capture that retries failures matching transientErrorMarkers.
func RunRetried(args []string, dryRun bool) (*Result, error) {
	return runRetried(args, dryRun, transientRetries, transientDelay)
}

func runRetried(args []string, dryRun bool, retries int, delay time.Duration) (*Result, error) {
	for attempt := 1; ; attempt++ {
		res, err := runFn(args, Opts{Capture: true, DryRun: dryRun})
		if err == nil {
			return res, nil
		}
		transient := false
		for _, marker := range transientErrorMarkers {
			if strings.Contains(err.Error(), marker) {
				transient = true
			}
		}
		if !transient || attempt >= retries {
			return res, err
		}
		fmt.Fprintf(os.Stderr, "wslc-compose: wslc %s hit a transient error, retrying in %gs (%d/%d)...\n",
			head(args, 2), delay.Seconds(), attempt, retries-1)
		sleepFn(delay)
	}
}

// Command prepares a wslc process for callers that need its output streamed.
func Command(args []string) (*exec.Cmd, error) {
	wslc, err := FindWslc()
	if err != nil {
		return nil, err
	}
	return command(wslc, args), nil
}

// command prepares a wslc process whose Wait gives up on its output pipes
// killDelay after it exited, should a descendant keep them open.
func command(wslc string, args []string) *exec.Cmd {
	cmd := exec.Command(wslc, args...)
	cmd.WaitDelay = killDelay
	return cmd
}

func CaptureJSON(args []string) (any, error) {
	res, err := Run(args, Opts{Capture: true})
	if err != nil {
		return nil, err
	}
	return decodeJSON(args, res.Stdout)
}

// decodeJSON decodes a JSON document, or a stream of them: wslc 3 prints one
// object per line for list and images, which is returned as a list.
func decodeJSON(args []string, stdout string) (any, error) {
	text := strings.TrimSpace(stdout)
	if text == "" {
		return []any{}, nil
	}
	dec := json.NewDecoder(strings.NewReader(text))
	var values []any
	for dec.More() {
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, &Error{fmt.Sprintf("unexpected non-JSON output from wslc %s: %v", strings.Join(args, " "), err)}
		}
		values = append(values, v)
	}
	if len(values) == 1 {
		return values[0], nil
	}
	return values, nil
}

// --- queries ---------------------------------------------------------------

func ListProjectContainers(project string) ([]map[string]any, error) {
	result, err := CaptureJSON([]string{"list", "-a", "--format", "json", "-f", "label=" + compose.LabelProject + "=" + project})
	if err != nil {
		return nil, err
	}
	return objects(result), nil
}

// objects returns the objects of a JSON list, or the object itself.
func objects(v any) []map[string]any {
	if m, ok := v.(map[string]any); ok {
		return []map[string]any{m}
	}
	list, _ := v.([]any)
	var out []map[string]any
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// Inspect returns the inspect data of a container, retrying transient errors.
func Inspect(id string) (map[string]any, error) {
	args := []string{"inspect", id}
	res, err := RunRetried(args, false)
	if err != nil {
		return nil, err
	}
	result, err := decodeJSON(args, res.Stdout)
	if err != nil {
		return nil, err
	}
	if m, ok := result.(map[string]any); ok {
		return m, nil
	}
	if list := objects(result); len(list) > 0 {
		return list[0], nil
	}
	return nil, &Error{fmt.Sprintf("wslc inspect %s returned no data", id)}
}

// namesFromTable parses names out of a wslc table output (network/volume list).
func namesFromTable(args []string, column string) ([]string, error) {
	res, err := Run(args, Opts{Capture: true})
	if err != nil {
		return nil, err
	}
	return parseTable(res.Stdout, column), nil
}

func parseTable(output, column string) []string {
	lines := strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n")
	idx := strings.Index(lines[0], column)
	if idx < 0 {
		return nil
	}
	var names []string
	for _, line := range lines[1:] {
		if len(line) > idx {
			if fields := strings.Fields(line[idx:]); len(fields) > 0 {
				names = append(names, fields[0])
			}
		}
	}
	return names
}

func ImageExists(name string) bool {
	repo, tag := name, ""
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		repo, tag = name[:i], name[i+1:]
	}
	images, err := CaptureJSON([]string{"images", "--format", "json"})
	if err != nil {
		return false
	}
	for _, img := range objects(images) {
		if img["Repository"] == repo && (tag == "" || img["Tag"] == tag) {
			return true
		}
	}
	return false
}

func NetworkNames() ([]string, error) { return resourceNames("network", "NAME") }

func VolumeNames() ([]string, error) { return resourceNames("volume", "VOLUME NAME") }

// resourceNames lists network or volume names from the JSON output of wslc 3,
// whose table headers are localized, or from the table of older versions.
func resourceNames(kind, column string) ([]string, error) {
	args := []string{kind, "list", "--format", "json"}
	res, err := Run(args, Opts{Capture: true, NoCheck: true})
	if err != nil {
		return nil, err
	}
	if res.Code == 0 {
		if result, err := decodeJSON(args, res.Stdout); err == nil {
			names := []string{}
			for _, item := range objects(result) {
				names = append(names, str(item["Name"]))
			}
			return names, nil
		}
	}
	return namesFromTable([]string{kind, "list"}, column)
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// EnsureNetwork creates the network unless it exists; reports whether it did.
func EnsureNetwork(name string, dryRun bool) (bool, error) {
	return ensure(NetworkNames, "network", name, dryRun)
}

// EnsureVolume creates the volume unless it exists; reports whether it did.
func EnsureVolume(name string, dryRun bool) (bool, error) {
	return ensure(VolumeNames, "volume", name, dryRun)
}

func ensure(list func() ([]string, error), kind, name string, dryRun bool) (bool, error) {
	existing, err := list()
	if err != nil {
		return false, err
	}
	if contains(existing, name) {
		return false, nil
	}
	_, err = Run([]string{kind, "create", name}, Opts{DryRun: dryRun})
	return err == nil, err
}
