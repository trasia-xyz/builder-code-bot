package notification

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

type Message struct {
	Status   Status
	Subject  string
	Body     string
	HTMLBody string
}

type Status string

const (
	StatusSuccess  Status = "success"
	StatusWarning  Status = "warning"
	StatusInfo     Status = "info"
	StatusRetrying Status = "retrying"
	StatusCritical Status = "critical"
)

func (s Status) Indicator() string {
	switch s {
	case StatusSuccess:
		return "🟢"
	case StatusWarning:
		return "🟡"
	case StatusInfo:
		return "🔵"
	case StatusRetrying:
		return "🟠"
	case StatusCritical:
		return "🔴"
	default:
		return ""
	}
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
	if strings.TrimSpace(m.Body) == "" && strings.TrimSpace(m.HTMLBody) == "" {
		errs = append(errs, fmt.Errorf("notification body is required"))
	}
	return errors.Join(errs...)
}
