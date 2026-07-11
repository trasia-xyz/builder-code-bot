package notification

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

type Message struct {
	Subject string
	Body    string
}

type Notifier interface {
	Notify(context.Context, Message) error
}

type Noop struct{}

func (Noop) Notify(context.Context, Message) error { return nil }

func (m Message) Validate() error {
	var errs []error
	if strings.TrimSpace(m.Subject) == "" {
		errs = append(errs, fmt.Errorf("notification subject is required"))
	}
	if strings.TrimSpace(m.Body) == "" {
		errs = append(errs, fmt.Errorf("notification body is required"))
	}
	return errors.Join(errs...)
}
