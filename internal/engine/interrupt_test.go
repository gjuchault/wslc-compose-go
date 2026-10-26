package engine

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// The test binary doubles as a fake wslc: with FAKE_WSLC=1 it sleeps
// FAKE_WSLC_SLEEP seconds and exits; with FAKE_WSLC_GRANDCHILD=secs it
// instead leaves behind a descendant holding its stdout for that long.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_WSLC") == "1" {
		if secs := os.Getenv("FAKE_WSLC_GRANDCHILD"); secs != "" {
			exe, _ := os.Executable()
			child := exec.Command(exe)
			child.Env = append(os.Environ(), "FAKE_WSLC_GRANDCHILD=", "FAKE_WSLC_SLEEP="+secs)
			child.Stdout = os.Stdout
			if child.Start() == nil && os.Getenv("FAKE_WSLC_PIDFILE") != "" {
				os.WriteFile(os.Getenv("FAKE_WSLC_PIDFILE"), []byte(strconv.Itoa(child.Process.Pid)), 0o644)
			}
			os.Exit(0)
		}
		secs, _ := strconv.ParseFloat(os.Getenv("FAKE_WSLC_SLEEP"), 64)
		time.Sleep(time.Duration(secs * float64(time.Second)))
		os.Exit(0)
	}
	exe, err := os.Executable()
	if err != nil {
		panic(err)
	}
	os.Setenv("WSLC_COMPOSE_BIN", exe)
	os.Setenv("FAKE_WSLC", "1")
	os.Exit(m.Run())
}

func TestRunStopsCapturedChildOnInterrupt(t *testing.T) {
	resetInterrupt()
	t.Cleanup(resetInterrupt)
	t.Setenv("FAKE_WSLC_SLEEP", "60")

	done := make(chan error, 1)
	go func() {
		_, err := Run([]string{"list"}, Opts{Capture: true})
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	Interrupt()

	select {
	case err := <-done:
		if !errors.Is(err, ErrInterrupted) {
			t.Errorf("err = %v, want ErrInterrupted", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return after an interrupt")
	}
}

func TestRunRefusesToStartOnceInterrupted(t *testing.T) {
	resetInterrupt()
	t.Cleanup(resetInterrupt)
	Interrupt()
	if _, err := Run([]string{"list"}, Opts{Capture: true}); !errors.Is(err, ErrInterrupted) {
		t.Errorf("err = %v, want ErrInterrupted", err)
	}
}

func TestRunWithoutInterruptStillWorks(t *testing.T) {
	resetInterrupt()
	t.Setenv("FAKE_WSLC_SLEEP", "0")
	if res, err := Run([]string{"list"}, Opts{Capture: true}); err != nil || res.Code != 0 {
		t.Errorf("res=%+v err=%v", res, err)
	}
}

func TestRunStopsAttachedChildOnInterrupt(t *testing.T) {
	resetInterrupt()
	t.Cleanup(resetInterrupt)
	t.Setenv("FAKE_WSLC_SLEEP", "60")

	done := make(chan error, 1)
	go func() {
		_, err := Run([]string{"exec", "web", "sh"}, Opts{})
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	Interrupt() // as SIGTERM would: the child is not signalled by the console

	select {
	case err := <-done:
		if !errors.Is(err, ErrInterrupted) {
			t.Errorf("err = %v, want ErrInterrupted", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run waited for an attached child that ignores the interrupt")
	}
}

func TestRunReturnsWhenADescendantHoldsTheOutput(t *testing.T) {
	resetInterrupt()
	t.Setenv("FAKE_WSLC_GRANDCHILD", "15")
	killDescendantAfter(t)
	begin := time.Now()
	if res, err := Run([]string{"list"}, Opts{Capture: true}); err != nil || res.Code != 0 {
		t.Errorf("res=%+v err=%v", res, err)
	}
	if elapsed := time.Since(begin); elapsed > killDelay+5*time.Second {
		t.Errorf("Run took %v, waiting for the descendant's output", elapsed)
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
