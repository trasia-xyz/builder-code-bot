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
		o.RetryStarted("list_pending", errors.New("password=hidden"))
		o.RetryProgress("list_pending", 8, 2*time.Minute+59*time.Second, errors.New("password=hidden"))
		o.RetryProgress("list_pending", 9, 3*time.Minute, errors.New("password=hidden"))
		o.RetryProgress("list_pending", 10, 4*time.Minute, errors.New("password=hidden"))
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
		if strings.Contains(message.Body, "password") {
			t.Fatalf("message leaked retry error: %q", message.Body)
		}
	}
}

func TestMySQLRetryObserverThrottlesProgressLogs(t *testing.T) {
	var output bytes.Buffer
	logger := logging.New(logging.Config{Format: logging.FormatJSON, Level: logging.LevelDebug, Output: &output})
	o := NewMySQLRetryObserver(NewDispatcher(Noop{}, logger), logger)
	err := errors.New("password=never-log-this")
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
