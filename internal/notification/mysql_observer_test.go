package notification

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"builder-code-bot/internal/logging"
)

func TestMySQLRetryObserverAlertsAndRecoversOncePerOutage(t *testing.T) {
	n := &recordingNotifier{}
	o := NewMySQLRetryObserver(NewDispatcher(n, logging.Logger{}), logging.Logger{})

	for incident := 0; incident < 2; incident++ {
		o.RetryStarted("list_pending", errors.New("password=hidden; database unavailable"))
		o.RetryProgress("list_pending", 8, 2*time.Minute+59*time.Second, errors.New("password=hidden; database unavailable"))
		o.RetryProgress("list_pending", 9, 3*time.Minute, errors.New("password=hidden; database unavailable"))
		o.RetryProgress("list_pending", 10, 4*time.Minute, errors.New("password=hidden; database unavailable"))
		o.Recovered("list_pending", 10, 4*time.Minute)
		o.Recovered("list_pending", 10, 4*time.Minute)
	}

	got := n.snapshot()
	if len(got) != 4 {
		t.Fatalf("messages = %d, want alert/recovery for two outages: %#v", len(got), got)
	}
	for i := 0; i < len(got); i += 2 {
		if got[i].Subject != "MySQL unavailable" || got[i+1].Subject != "MySQL recovered" {
			t.Fatalf("messages = %#v", got)
		}
	}
	for _, message := range got {
		if strings.Contains(message.Body, "password") || strings.Contains(message.Body, "database unavailable") {
			t.Fatalf("message leaked retry error: %q", message.Body)
		}
	}
}

func TestMySQLRetryObserverThrottlesProgressLogs(t *testing.T) {
	var output bytes.Buffer
	logger := logging.New(logging.Config{Format: logging.FormatJSON, Level: logging.LevelDebug, Output: &output})
	o := NewMySQLRetryObserver(NewDispatcher(Noop{}, logger), logger)
	err := errors.New("password=never-log-this; database unavailable")
	o.RetryStarted("complete", err)
	o.RetryProgress("complete", 2, 30*time.Second, err)
	o.RetryProgress("complete", 3, time.Minute, err)
	o.RetryProgress("complete", 4, 90*time.Second, err)
	o.RetryProgress("complete", 5, 2*time.Minute, err)
	o.Recovered("complete", 5, 2*time.Minute)
	logs := output.String()
	if got := strings.Count(logs, `"event":"mysql_retry_progress"`); got != 2 {
		t.Fatalf("progress logs = %d, want 2: %s", got, logs)
	}
	if strings.Contains(logs, "never-log-this") {
		t.Fatalf("logs leaked retry error: %s", logs)
	}
	if got := strings.Count(logs, "database unavailable"); got != 3 {
		t.Fatalf("logged retry causes = %d, want start plus two progress logs: %s", got, logs)
	}
}

func TestMySQLRetryObserverNoRecoveryEmailBeforeAlertThreshold(t *testing.T) {
	n := &recordingNotifier{}
	o := NewMySQLRetryObserver(NewDispatcher(n, logging.Logger{}), logging.Logger{})
	o.RetryStarted("complete", errors.New("unavailable"))
	o.RetryProgress("complete", 2, time.Minute, errors.New("unavailable"))
	o.Recovered("complete", 2, time.Minute)
	if got := n.snapshot(); len(got) != 0 {
		t.Fatalf("messages = %#v", got)
	}
}

func TestMySQLRetryObserverLogsSafeDependencyDiagnostic(t *testing.T) {
	var output bytes.Buffer
	logger := logging.New(logging.Config{Format: logging.FormatJSON, Level: logging.LevelDebug, Output: &output})
	o := NewMySQLRetryObserver(NewDispatcher(Noop{}, logger), logger)

	o.Diagnostic("complete_payout", "commit_ambiguous",
		errors.New("password=do-not-log; connection reset during commit"))

	logs := output.String()
	if strings.Contains(logs, "do-not-log") {
		t.Fatalf("diagnostic log leaked retry error: %s", logs)
	}
	for _, want := range []string{
		`"event":"mysql_dependency_diagnostic"`,
		`"operation":"complete_payout"`,
		`"stage":"commit_ambiguous"`,
		"connection reset during commit",
	} {
		if !strings.Contains(logs, want) {
			t.Fatalf("diagnostic log missing %q: %s", want, logs)
		}
	}
}

func TestMySQLRetryObserverTracksConcurrentOperationsIndependently(t *testing.T) {
	var output bytes.Buffer
	logger := logging.New(logging.Config{
		Format: logging.FormatJSON, Level: logging.LevelDebug, Output: &output,
	})
	o := NewMySQLRetryObserver(NewDispatcher(Noop{}, logger), logger)

	o.RetryStarted("list_points", errors.New("points connection refused"))
	o.RetryStarted("list_referral", errors.New("referral connection refused"))
	o.RetryProgress("list_points", 2, time.Minute, errors.New("points still unavailable"))
	o.RetryProgress("list_referral", 2, time.Minute, errors.New("referral still unavailable"))
	o.Recovered("list_points", 2, time.Minute)
	o.Recovered("list_referral", 2, time.Minute)

	logs := output.String()
	for _, want := range []string{
		"points connection refused", "referral connection refused",
		"points still unavailable", "referral still unavailable",
	} {
		if !strings.Contains(logs, want) {
			t.Fatalf("concurrent retry logs missing %q: %s", want, logs)
		}
	}
	if got := strings.Count(logs, `"event":"mysql_retry_started"`); got != 2 {
		t.Fatalf("retry-start logs = %d, want 2: %s", got, logs)
	}
	if got := strings.Count(logs, `"event":"mysql_retry_progress"`); got != 2 {
		t.Fatalf("retry-progress logs = %d, want 2: %s", got, logs)
	}
}
