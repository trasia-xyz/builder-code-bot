package notification

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"hyperliquid-builder-code-bot/internal/logging"
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

func TestDispatcherSuppressesDuplicateActiveAlert(t *testing.T) {
	t.Parallel()
	n := &recordingNotifier{}
	d := NewDispatcher(n)
	d.Alert(context.Background(), "mysql_down", Message{Subject: "mysql down", Body: "body"})
	d.Alert(context.Background(), "mysql_down", Message{Subject: "mysql still down", Body: "body"})
	if got := len(n.snapshot()); got != 1 {
		t.Fatalf("messages = %d, want 1", got)
	}
}

func TestDispatcherResolveClosesDedupeWindow(t *testing.T) {
	t.Parallel()
	n := &recordingNotifier{}
	d := NewDispatcher(n)
	d.Resolve(context.Background(), "mysql_down", Message{Subject: "unused", Body: "unused"})
	d.Alert(context.Background(), "mysql_down", Message{Subject: "down", Body: "body"})
	d.Resolve(context.Background(), "mysql_down", Message{Subject: "recovered", Body: "body"})
	d.Resolve(context.Background(), "mysql_down", Message{Subject: "duplicate recovery", Body: "body"})
	d.Alert(context.Background(), "mysql_down", Message{Subject: "down again", Body: "body"})
	got := n.snapshot()
	if len(got) != 3 || got[0].Subject != "down" || got[1].Subject != "recovered" || got[2].Subject != "down again" {
		t.Fatalf("messages = %#v", got)
	}
}

func TestDispatcherReportsEveryInvocation(t *testing.T) {
	t.Parallel()
	n := &recordingNotifier{}
	d := NewDispatcher(n)
	message := Message{Subject: "funding run succeeded", Body: "status: succeeded"}
	d.Report(context.Background(), message)
	d.Report(context.Background(), message)
	if got := len(n.snapshot()); got != 2 {
		t.Fatalf("messages = %d, want 2", got)
	}
}

func TestDispatcherDedupeIsConcurrentAndFailureDoesNotEscape(t *testing.T) {
	t.Parallel()
	const secret = "password=do-not-log"
	var output bytes.Buffer
	n := &recordingNotifier{err: errors.New(secret)}
	d := NewDispatcher(n, logging.New(logging.Config{Format: logging.FormatJSON, Level: logging.LevelDebug, Output: &output}))
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.Alert(context.Background(), "same", Message{Subject: "down", Body: "body"})
		}()
	}
	wg.Wait()
	if got := len(n.snapshot()); got != 1 {
		t.Fatalf("messages = %d, want 1", got)
	}
	if strings.Contains(output.String(), secret) {
		t.Fatalf("log leaked notifier error: %s", output.String())
	}
}
