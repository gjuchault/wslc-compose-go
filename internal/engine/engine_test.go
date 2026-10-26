package engine

import (
	"reflect"
	"testing"
	"time"
)

// fakeRun replaces runFn with a stub failing with fail(call) and counts calls.
func fakeRun(t *testing.T, fail func(call int) error) *int {
	t.Helper()
	calls := 0
	origRun, origSleep := runFn, sleepFn
	t.Cleanup(func() { runFn, sleepFn = origRun, origSleep })
	sleepFn = func(time.Duration) {}
	runFn = func(args []string, o Opts) (*Result, error) {
		calls++
		if err := fail(calls); err != nil {
			return &Result{Code: 1}, err
		}
		return &Result{}, nil
	}
	return &calls
}

func TestRunRetriedRecoversFromTransientError(t *testing.T) {
	calls := fakeRun(t, func(call int) error {
		if call == 1 {
			return &Error{"wslc start foo failed (exit 1): Impossible de créer un fichier " +
				"déjà existant. Code d'erreur : ERROR_ALREADY_EXISTS"}
		}
		return nil
	})
	res, err := RunRetried([]string{"start", "foo"}, false)
	if err != nil || res.Code != 0 || *calls != 2 {
		t.Errorf("res=%+v err=%v calls=%d", res, err, *calls)
	}
}

func TestRunRetriedGivesUpAfterMaxAttempts(t *testing.T) {
	calls := fakeRun(t, func(int) error {
		return &Error{"wslc start foo failed (exit 1): ERROR_SHARING_VIOLATION"}
	})
	if _, err := runRetried([]string{"start", "foo"}, false, 3, 0); err == nil || *calls != 3 {
		t.Errorf("err=%v calls=%d", err, *calls)
	}
}

func TestRunRetriedDoesNotRetryOtherErrors(t *testing.T) {
	calls := fakeRun(t, func(int) error {
		return &Error{"wslc start foo failed (exit 1): no such container"}
	})
	if _, err := RunRetried([]string{"start", "foo"}, false); err == nil || *calls != 1 {
		t.Errorf("err=%v calls=%d", err, *calls)
	}
}

func TestParseTable(t *testing.T) {
	out := "DRIVER   VOLUME NAME\r\nlocal    demo_data\r\nlocal    other\r\n"
	if got := parseTable(out, "VOLUME NAME"); !reflect.DeepEqual(got, []string{"demo_data", "other"}) {
		t.Errorf("got %q", got)
	}
	if got := parseTable("", "NAME"); got != nil {
		t.Errorf("got %q", got)
	}
}

func TestInspectRetriesTransientErrors(t *testing.T) {
	calls := fakeRun(t, func(call int) error {
		if call == 1 {
			return &Error{"wslc inspect abc failed (exit 1): ERROR_SHARING_VIOLATION"}
		}
		return nil
	})
	inner := runFn
	runFn = func(args []string, o Opts) (*Result, error) {
		res, err := inner(args, o)
		if err == nil {
			res.Stdout = `[{"Name":"demo-web-1"}]`
		}
		return res, err
	}
	data, err := Inspect("abc")
	if err != nil || data["Name"] != "demo-web-1" || *calls != 2 {
		t.Errorf("data=%v err=%v calls=%d", data, err, *calls)
	}
}
