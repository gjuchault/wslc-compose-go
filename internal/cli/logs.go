package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"wslc-go/internal/compose"
	"wslc-go/internal/engine"
)

type logOptions struct {
	follow     bool
	tail       int // < 0: all lines
	timestamps bool
}

// newCommand prepares a wslc process; replaced in tests.
var newCommand = engine.Command

// followLogs streams the logs of the project's containers (all, or those of
// services), each line prefixed with its container name.
func followLogs(project *compose.Project, services []string, opts logOptions) (int, error) {
	all, err := projectContainers(project, false, nil)
	if err != nil {
		return 1, err
	}
	containers := filterContainers(all, services)
	if len(containers) == 0 {
		errorMsg("no containers found")
		return 1, nil
	}
	return followContainers(containers, opts, engine.Interrupted())
}

type follower struct {
	name   string
	cmd    *exec.Cmd
	exited chan struct{} // closed once cmd has been waited for
	err    error
}

// prefixWriter prints whole lines prefixed with a container name; mu keeps
// the lines of concurrent followers from interleaving.
type prefixWriter struct {
	prefix  string
	mu      *sync.Mutex
	partial []byte
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	n := len(p)
	for {
		i := bytes.IndexByte(p, '\n') // partial has none: only scan the new bytes
		if i < 0 {
			w.partial = append(w.partial, p...)
			return n, nil
		}
		w.print(append(w.partial, p[:i+1]...))
		w.partial = w.partial[:0]
		p = p[i+1:]
	}
}

// flush prints an unterminated last line.
func (w *prefixWriter) flush() {
	if len(w.partial) > 0 {
		w.print(append(w.partial, '\n'))
		w.partial = nil
	}
}

func (w *prefixWriter) print(line []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	os.Stdout.WriteString(w.prefix + string(line))
}

// followContainers runs one `wslc logs` per container until they all end or
// interrupted is closed. An interrupt stops the followers and is not an error;
// every started follower is reaped before returning.
func followContainers(containers []container, opts logOptions, interrupted <-chan struct{}) (int, error) {
	width := 0
	for _, c := range containers {
		width = max(width, len(c.Name))
	}

	var (
		mu        sync.Mutex
		followers []*follower
	)
	stopAll := func() {
		var wg sync.WaitGroup
		for _, f := range followers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				engine.Stop(f.cmd.Process, f.exited)
				<-f.exited
			}()
		}
		wg.Wait()
	}

	for _, c := range containers {
		args := []string{"logs"}
		if opts.follow && c.Running {
			args = append(args, "-f")
		}
		if opts.tail >= 0 {
			args = append(args, "-n", strconv.Itoa(opts.tail))
		}
		if opts.timestamps {
			args = append(args, "-t")
		}
		args = append(args, c.Name)

		cmd, err := newCommand(args)
		if err != nil {
			stopAll()
			return 1, err
		}
		// a writer rather than StdoutPipe: Wait then returns once the process
		// exited (after cmd.WaitDelay), even if a descendant keeps the pipe open
		out := &prefixWriter{prefix: fmt.Sprintf("%-*s | ", width, c.Name), mu: &mu}
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Start(); err != nil {
			stopAll()
			return 1, &engine.Error{Msg: fmt.Sprintf("cannot follow logs of %s: %v", c.Name, err)}
		}

		f := &follower{name: c.Name, cmd: cmd, exited: make(chan struct{})}
		followers = append(followers, f)
		go func() {
			defer close(f.exited)
			f.err = cmd.Wait()
			if errors.Is(f.err, exec.ErrWaitDelay) {
				f.err = nil
			}
			out.flush()
		}()
	}

	allExited := make(chan struct{})
	go func() {
		for _, f := range followers {
			<-f.exited
		}
		close(allExited)
	}()
	select {
	case <-allExited:
		select {
		case <-interrupted: // the same Ctrl+C ended the followers
			return 0, nil
		default:
		}
	case <-interrupted:
		stopAll()
		return 0, nil
	}

	var failed []string
	for _, f := range followers {
		if f.err != nil {
			failed = append(failed, f.name)
		}
	}
	if len(failed) > 0 {
		return 1, &engine.Error{Msg: "wslc logs failed for " + strings.Join(failed, ", ")}
	}
	return 0, nil
}
