package notification

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"hyperliquid-builder-code-bot/internal/logging"
	"hyperliquid-builder-code-bot/internal/repository/mysql"
)

const (
	mysqlAlertKey         = "mysql_down"
	mysqlAlertThreshold   = 3 * time.Minute
	mysqlProgressInterval = time.Minute
)

type Dispatcher struct {
	notifier Notifier
	logger   logging.Logger
}

func NewDispatcher(notifier Notifier, loggers ...logging.Logger) *Dispatcher {
	if notifier == nil {
		notifier = Noop{}
	}
	var logger logging.Logger
	if len(loggers) > 0 {
		logger = loggers[0]
	}
	return &Dispatcher{notifier: notifier, logger: logger}
}

// Alert sends a funding or operational alert. Callers own any incident-specific
// deduplication state.
func (d *Dispatcher) Alert(ctx context.Context, key string, message Message) {
	if d == nil || key == "" {
		return
	}
	if err := d.notifier.Notify(ctx, message); err != nil {
		d.logger.Error(ctx, "notification delivery failed",
			logging.String("event", "notification_delivery_failed"),
			logging.String("alert_key", key),
		)
	}
}

// Report sends a notification without opening a deduplication window. Reports
// describe individual funding runs, so every invocation must be delivered.
func (d *Dispatcher) Report(ctx context.Context, message Message) {
	if d == nil {
		return
	}
	if err := d.notifier.Notify(ctx, message); err != nil {
		d.logger.Error(ctx, "notification delivery failed",
			logging.String("event", "notification_delivery_failed"),
			logging.String("notification_kind", "funding_run_report"),
		)
	}
}

// MySQLRetryObserver adapts repository retry events into throttled logs and a
type MySQLRetryObserver struct {
	mu sync.Mutex

	dispatcher   *Dispatcher
	logger       logging.Logger
	outage       bool
	alerted      bool
	lastProgress time.Duration
}

var _ mysql.RetryObserver = (*MySQLRetryObserver)(nil)

func NewMySQLRetryObserver(
	dispatcher *Dispatcher,
	logger logging.Logger,
) *MySQLRetryObserver {
	if dispatcher == nil {
		dispatcher = NewDispatcher(Noop{}, logger)
	}
	return &MySQLRetryObserver{dispatcher: dispatcher, logger: logger}
}

func (o *MySQLRetryObserver) RetryStarted(operation string, _ error) {
	if o == nil {
		return
	}
	o.mu.Lock()
	if o.outage {
		o.mu.Unlock()
		return
	}
	o.outage = true
	o.alerted = false
	o.lastProgress = 0
	o.mu.Unlock()
	o.logger.Warn(context.Background(), "MySQL retry started",
		logging.String("event", "mysql_retry_started"),
		logging.String("operation", operation),
	)
}

func (o *MySQLRetryObserver) RetryProgress(operation string, attempts int, unavailableFor time.Duration, _ error) {
	if o == nil {
		return
	}
	o.mu.Lock()
	if !o.outage {
		o.outage = true
	}
	shouldLog := unavailableFor >= o.lastProgress+mysqlProgressInterval
	if shouldLog {
		o.lastProgress = unavailableFor
	}
	shouldAlert := unavailableFor >= mysqlAlertThreshold && !o.alerted
	if shouldAlert {
		o.alerted = true
	}
	o.mu.Unlock()
	if shouldAlert {
		o.dispatcher.Alert(context.Background(), mysqlAlertKey, mysqlMessage("MySQL unavailable", operation, attempts, unavailableFor))
	}
	if shouldLog {
		o.logger.Warn(context.Background(), "MySQL retry in progress",
			logging.String("event", "mysql_retry_progress"),
			logging.String("operation", operation),
			logging.Int("attempts", attempts),
			logging.Duration("unavailable_duration", unavailableFor),
		)
	}
}

func (o *MySQLRetryObserver) Recovered(operation string, attempts int, unavailableFor time.Duration) {
	if o == nil {
		return
	}
	o.mu.Lock()
	if !o.outage {
		o.mu.Unlock()
		return
	}
	wasAlerted := o.alerted
	o.outage = false
	o.alerted = false
	o.lastProgress = 0
	o.mu.Unlock()
	if wasAlerted {
		o.dispatcher.Alert(context.Background(), mysqlAlertKey, mysqlMessage("MySQL recovered", operation, attempts, unavailableFor))
	}
	o.logger.Info(context.Background(), "MySQL connection recovered",
		logging.String("event", "mysql_connection_recovered"),
		logging.String("operation", operation),
		logging.Int("attempts", attempts),
		logging.Duration("unavailable_duration", unavailableFor),
	)
}

func mysqlMessage(subject, operation string, attempts int, unavailableFor time.Duration) Message {
	return Message{
		Subject: subject,
		Body: strings.Join([]string{
			"operation: " + operation,
			"attempts: " + strconv.Itoa(attempts),
			"unavailable duration: " + unavailableFor.String(),
		}, "\n"),
	}
}
