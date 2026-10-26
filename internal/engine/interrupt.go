package engine

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// killDelay is how long a stopped child gets to exit before being killed.
const killDelay = 3 * time.Second

var (
	interruptMu   sync.Mutex
	interruptCh   = make(chan struct{})
	interruptOnce = new(sync.Once)
	handlerOnce   sync.Once
)

// Interrupted is closed once the user asked to stop (Ctrl+C or SIGTERM).
func Interrupted() <-chan struct{} {
	interruptMu.Lock()
	defer interruptMu.Unlock()
	return interruptCh
}

// Interrupt behaves as if Ctrl+C was pressed: running wslc commands are
// stopped and new ones refuse to start.
func Interrupt() {
	interruptMu.Lock()
	defer interruptMu.Unlock()
	interruptOnce.Do(func() { close(interruptCh) })
}

func resetInterrupt() {
	interruptMu.Lock()
	defer interruptMu.Unlock()
	interruptCh = make(chan struct{})
	interruptOnce = new(sync.Once)
}

// HandleInterrupts routes Ctrl+C/SIGTERM to Interrupt. A second signal exits
// immediately with status 130, in case a child refuses to stop.
func HandleInterrupts() {
	handlerOnce.Do(func() {
		sig := make(chan os.Signal, 2)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-sig
			Interrupt()
			<-sig
			fmt.Fprintln(os.Stderr, "wslc-compose: aborted")
			os.Exit(130)
		}()
	})
}

// Stop asks a child to terminate (SIGTERM, or TerminateProcess on Windows)
// and kills it if it has not exited after killDelay. exited must be closed
// once the child has been waited for.
func Stop(p *os.Process, exited <-chan struct{}) {
	if terminate(p) != nil {
		p.Kill()
		return
	}
	select {
	case <-exited:
	case <-time.After(killDelay):
		p.Kill()
	}
}

// interruptGrace is how long a child, which shares our console and so received
// the Ctrl+C too, gets to exit on its own before being stopped, like with
// Python's subprocess.run.
const interruptGrace = 250 * time.Millisecond

// wait waits for a started cmd and stops it on interrupt, after
// interruptGrace. A SIGTERM only reaches us, so it is passed on that way.
func wait(cmd *exec.Cmd) (err error, interrupted bool) {
	exited := make(chan struct{})
	go func() {
		err = cmd.Wait()
		close(exited)
	}()
	select {
	case <-exited:
		return err, false
	case <-Interrupted():
	}
	select {
	case <-exited:
		return err, true
	case <-time.After(interruptGrace):
	}
	Stop(cmd.Process, exited)
	<-exited
	return err, true
}
