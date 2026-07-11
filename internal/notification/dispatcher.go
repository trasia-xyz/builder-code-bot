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
	mu       sync.Mutex
	notifier Notifier
	logger   logging.Logger
	active   map[string]struct{}
}

func NewDispatcher(notifier Notifier, loggers ...logging.Logger) *Dispatcher {
	if notifier == nil {
		notifier = Noop{}
	}
	var logger logging.Logger
	if len(loggers) > 0 {
		logger = loggers[0]
	}
	return &Dispatcher{
		notifier: notifier,
		logger:   logger,
		active:   make(map[string]struct{}),
	}
}

// Alert sends at most one notification for a key until Resolve closes its
// active window. Delivery failures are contained and do not reopen the window.
func (d *Dispatcher) Alert(ctx context.Context, key string, message Message) {
	if d == nil || key == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.active[key]; ok {
		return
	}
	d.active[key] = struct{}{}
	if err := d.notifier.Notify(ctx, message); err != nil {
		d.logger.Error(ctx, "notification delivery failed",
			logging.String("event", "notification_delivery_failed"),
			logging.String("alert_key", key),
		)
	}
}

// Resolve sends a recovery notification only for an active key and then
// closes that key's window, allowing a later incident to alert again.
func (d *Dispatcher) Resolve(ctx context.Context, key string, message Message) {
	if d == nil || key == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.active[key]; !ok {
		return
	}
	delete(d.active, key)
	if err := d.notifier.Notify(ctx, message); err != nil {
		d.logger.Error(ctx, "notification delivery failed",
			logging.String("event", "notification_delivery_failed"),
			logging.String("alert_key", key),
		)
	}
}

type retryWindow struct {
	lastProgress time.Duration
}

// MySQLRetryObserver adapts repository retry events into throttled logs and a
// deduplicated outage/recovery notification window.
type MySQLRetryObserver struct {
	mu         sync.Mutex
	dispatcher *Dispatcher
	logger     logging.Logger
	windows    map[string]retryWindow
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
		dispatcher: dispatcher,
		logger:     logger,
		windows:    make(map[string]retryWindow),
	}
}

func (o *MySQLRetryObserver) RetryStarted(operation string, _ error) {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.windows[operation] = retryWindow{}
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
	window := o.windows[operation]
	shouldLog := unavailableFor >= window.lastProgress+mysqlProgressInterval
	if shouldLog {
		window.lastProgress = unavailableFor
		o.windows[operation] = window
	}
	// Keep the observer and dispatcher transitions under one fixed lock order
	// so a recovery or a newly started operation cannot split this global
	// outage window between its state check and Alert.
	if unavailableFor >= mysqlAlertThreshold {
		o.dispatcher.Alert(context.Background(), mysqlAlertKey, mysqlMessage("MySQL unavailable", operation, attempts, unavailableFor))
	}
	o.mu.Unlock()
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
	_, known := o.windows[operation]
	delete(o.windows, operation)
	allRecovered := len(o.windows) == 0
	if !known {
		o.mu.Unlock()
		return
	}
	// The final window removal and Dispatcher resolution are one transition.
	// Dispatcher never calls back into the observer, so observer -> dispatcher
	// is the only lock order.
	if allRecovered {
		o.dispatcher.Resolve(context.Background(), mysqlAlertKey, mysqlMessage("MySQL recovered", operation, attempts, unavailableFor))
	}
	o.mu.Unlock()
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
