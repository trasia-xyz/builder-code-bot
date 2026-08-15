package notification

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"builder-code-bot/internal/logging"
	"builder-code-bot/internal/repository/mysql"
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

func NewDispatcher(notifier Notifier, logger logging.Logger) *Dispatcher {
	if notifier == nil {
		notifier = Noop{}
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
			logging.Err(err),
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
			logging.Err(err),
		)
	}
}

// MySQLRetryObserver adapts repository retry events into throttled logs and a
type MySQLRetryObserver struct {
	mu sync.Mutex

	dispatcher *Dispatcher
	logger     logging.Logger
	outages    map[string]time.Duration
	alerted    bool
}

var _ mysql.RetryObserver = (*MySQLRetryObserver)(nil)

func NewMySQLRetryObserver(
	dispatcher *Dispatcher,
	logger logging.Logger,
) *MySQLRetryObserver {
	if dispatcher == nil {
		dispatcher = NewDispatcher(Noop{}, logger)
	}
	return &MySQLRetryObserver{
		dispatcher: dispatcher, logger: logger,
		outages: make(map[string]time.Duration),
	}
}

func (o *MySQLRetryObserver) RetryStarted(operation string, err error) {
	if o == nil {
		return
	}
	o.mu.Lock()
	if _, exists := o.outages[operation]; exists {
		o.mu.Unlock()
		return
	}
	if len(o.outages) == 0 {
		o.alerted = false
	}
	o.outages[operation] = 0
	o.mu.Unlock()
	o.logger.Warn(context.Background(), "MySQL retry started",
		logging.String("event", "mysql_retry_started"),
		logging.String("operation", operation),
		logging.Err(err),
	)
}

func (o *MySQLRetryObserver) RetryProgress(operation string, attempts int, unavailableFor time.Duration, err error) {
	if o == nil {
		return
	}
	o.mu.Lock()
	lastProgress, exists := o.outages[operation]
	if !exists {
		o.outages[operation] = 0
	}
	shouldLog := unavailableFor >= lastProgress+mysqlProgressInterval
	if shouldLog {
		o.outages[operation] = unavailableFor
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
			logging.Err(err),
		)
	}
}

// Diagnostic records a dependency failure that does not belong to the normal
// retry lifecycle, such as an ambiguous commit outcome. Retry users discover
// this method through an optional interface so existing observers stay
// compatible.
func (o *MySQLRetryObserver) Diagnostic(operation, stage string, err error) {
	if o == nil {
		return
	}
	o.logger.Warn(context.Background(), "MySQL dependency diagnostic",
		logging.String("event", "mysql_dependency_diagnostic"),
		logging.String("operation", operation),
		logging.String("stage", stage),
		logging.Err(err),
	)
}

func (o *MySQLRetryObserver) Recovered(operation string, attempts int, unavailableFor time.Duration) {
	if o == nil {
		return
	}
	o.mu.Lock()
	if _, exists := o.outages[operation]; !exists {
		o.mu.Unlock()
		return
	}
	delete(o.outages, operation)
	wasLastAlertedOutage := o.alerted && len(o.outages) == 0
	if len(o.outages) == 0 {
		o.alerted = false
	}
	o.mu.Unlock()
	if wasLastAlertedOutage {
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
	status := StatusCritical
	if subject == "MySQL recovered" {
		status = StatusSuccess
	}
	return Message{
		Status:  status,
		Subject: subject,
		Body: strings.Join([]string{
			"operation: " + operation,
			"attempts: " + strconv.Itoa(attempts),
			"unavailable duration: " + unavailableFor.String(),
		}, "\n"),
	}
}
