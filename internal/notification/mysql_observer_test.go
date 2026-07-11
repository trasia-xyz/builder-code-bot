package notification

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"hyperliquid-builder-code-bot/internal/logging"
)

func TestMySQLRetryObserverAlertsAtThresholdAndResolvesOnce(t *testing.T) {
	t.Parallel()
	n := &recordingNotifier{}
	d := NewDispatcher(n)
	o := NewMySQLRetryObserver(d, logging.Logger{})
	o.RetryStarted("list_pending", errors.New("password=hidden"))
	o.RetryProgress("list_pending", 8, 2*time.Minute+59*time.Second, errors.New("password=hidden"))
	o.RetryProgress("list_pending", 9, 3*time.Minute, errors.New("password=hidden"))
	o.RetryProgress("list_pending", 10, 4*time.Minute, errors.New("password=hidden"))
	o.Recovered("list_pending", 10, 4*time.Minute)
	o.Recovered("list_pending", 10, 4*time.Minute)
	got := n.snapshot()
	if len(got) != 2 {
		t.Fatalf("messages = %d, want alert and recovery: %#v", len(got), got)
	}
	if !strings.Contains(got[0].Body, "attempts: 9") || !strings.Contains(got[0].Body, "unavailable duration: 3m0s") {
		t.Fatalf("alert body = %q", got[0].Body)
	}
	if !strings.Contains(got[1].Body, "attempts: 10") || !strings.Contains(got[1].Body, "unavailable duration: 4m0s") {
		t.Fatalf("recovery body = %q", got[1].Body)
	}
	for _, message := range got {
		if strings.Contains(message.Body, "password") {
			t.Fatalf("message leaked retry error: %q", message.Body)
		}
	}
}

func TestMySQLRetryObserverThrottlesProgressLogsToOneMinute(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	logger := logging.New(logging.Config{Format: logging.FormatJSON, Level: logging.LevelDebug, Output: &output})
	o := NewMySQLRetryObserver(NewDispatcher(Noop{}, logger), logger)
	wantSecret := "password=never-log-this"
	err := errors.New(wantSecret)
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
	if strings.Contains(logs, wantSecret) {
		t.Fatalf("logs leaked retry error: %s", logs)
	}
}

func TestMySQLRetryObserverResolvesAfterLastConcurrentOperationRecovers(t *testing.T) {
	t.Parallel()
	n := &recordingNotifier{}
	o := NewMySQLRetryObserver(NewDispatcher(n), logging.Logger{})
	o.RetryStarted("list_pending", errors.New("unavailable"))
	o.RetryStarted("acquire_run_lock", errors.New("unavailable"))
	o.RetryProgress("list_pending", 9, 3*time.Minute, errors.New("unavailable"))
	o.RetryProgress("acquire_run_lock", 9, 3*time.Minute, errors.New("unavailable"))
	o.Recovered("list_pending", 9, 3*time.Minute)
	o.RetryProgress("acquire_run_lock", 10, 4*time.Minute, errors.New("unavailable"))
	if got := len(n.snapshot()); got != 1 {
		t.Fatalf("messages before final recovery = %d, want one active alert", got)
	}
	o.Recovered("acquire_run_lock", 10, 4*time.Minute)
	got := n.snapshot()
	if len(got) != 2 || got[1].Subject != "MySQL recovered" {
		t.Fatalf("messages after final recovery = %#v", got)
	}
}

func TestMySQLRetryObserverSerializesWindowStartWithAlertTransition(t *testing.T) {
	t.Parallel()
	n := newBlockingNotifier(1)
	o := NewMySQLRetryObserver(NewDispatcher(n), logging.Logger{})
	o.RetryStarted("first", errors.New("unavailable"))
	progressDone := make(chan struct{})
	go func() {
		o.RetryProgress("first", 9, 3*time.Minute, errors.New("unavailable"))
		close(progressDone)
	}()
	<-n.entered
	started := make(chan struct{})
	go func() {
		close(started)
		o.RetryStarted("second", errors.New("unavailable"))
		close(n.windowChanged)
	}()
	<-started
	assertBlocked(t, n.windowChanged)
	close(n.release)
	<-progressDone
	<-n.windowChanged
}

func TestMySQLRetryObserverSerializesWindowStartWithResolveTransition(t *testing.T) {
	t.Parallel()
	n := newBlockingNotifier(2)
	o := NewMySQLRetryObserver(NewDispatcher(n), logging.Logger{})
	o.RetryStarted("first", errors.New("unavailable"))
	o.RetryProgress("first", 9, 3*time.Minute, errors.New("unavailable"))
	recoveredDone := make(chan struct{})
	go func() {
		o.Recovered("first", 9, 3*time.Minute)
		close(recoveredDone)
	}()
	<-n.entered
	started := make(chan struct{})
	go func() {
		close(started)
		o.RetryStarted("second", errors.New("unavailable"))
		close(n.windowChanged)
	}()
	<-started
	assertBlocked(t, n.windowChanged)
	close(n.release)
	<-recoveredDone
	<-n.windowChanged
}

func assertBlocked(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
		t.Fatal("retry window changed during dispatcher transition")
	case <-time.After(50 * time.Millisecond):
	}
}

type blockingNotifier struct {
	mu            sync.Mutex
	blockCall     int
	calls         int
	entered       chan struct{}
	release       chan struct{}
	windowChanged chan struct{}
}

func newBlockingNotifier(blockCall int) *blockingNotifier {
	return &blockingNotifier{
		blockCall: blockCall, entered: make(chan struct{}), release: make(chan struct{}),
		windowChanged: make(chan struct{}),
	}
}

func (n *blockingNotifier) Notify(context.Context, Message) error {
	n.mu.Lock()
	n.calls++
	call := n.calls
	n.mu.Unlock()
	if call == n.blockCall {
		close(n.entered)
		<-n.release
	}
	return nil
}
