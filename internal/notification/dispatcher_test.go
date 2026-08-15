package notification

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"builder-code-bot/internal/logging"
)

type recordingNotifier struct {
	mu       sync.Mutex
	messages []Message
	err      error
}

func (n *recordingNotifier) Notify(_ context.Context, message Message) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.messages = append(n.messages, message)
	return n.err
}

func (n *recordingNotifier) snapshot() []Message {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]Message(nil), n.messages...)
}

func TestDispatcherSendsEveryFundingAlertAndReport(t *testing.T) {
	n := &recordingNotifier{}
	d := NewDispatcher(n, logging.Logger{})
	d.Alert(context.Background(), "first", Message{Subject: "alert"})
	d.Alert(context.Background(), "first", Message{Subject: "alert again"})
	d.Report(context.Background(), Message{Subject: "report"})
	got := n.snapshot()
	if len(got) != 3 || got[0].Subject != "alert" || got[1].Subject != "alert again" || got[2].Subject != "report" {
		t.Fatalf("messages = %#v", got)
	}
}

func TestDispatcherLogsSafeDeliveryFailureCause(t *testing.T) {
	const secret = "password=do-not-log"
	var output bytes.Buffer
	n := &recordingNotifier{err: errors.New(secret + "; SMTP connection refused")}
	d := NewDispatcher(n, logging.New(logging.Config{Format: logging.FormatJSON, Level: logging.LevelDebug, Output: &output}))
	d.Alert(context.Background(), "funding", Message{Subject: "down"})
	d.Report(context.Background(), Message{Subject: "report"})
	if len(n.snapshot()) != 2 {
		t.Fatal("notifications were not attempted")
	}
	if strings.Contains(output.String(), secret) {
		t.Fatalf("log leaked notifier error: %s", output.String())
	}
	if got := strings.Count(output.String(), "SMTP connection refused"); got != 2 {
		t.Fatalf("safe notifier cause count = %d, want 2: %s", got, output.String())
	}
	if got := strings.Count(output.String(), `"error":`); got != 2 {
		t.Fatalf("error fields = %d, want 2: %s", got, output.String())
	}
}
