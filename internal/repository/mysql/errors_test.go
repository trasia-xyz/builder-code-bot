package mysql

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"

	drivermysql "github.com/go-sql-driver/mysql"

	"builder-code-bot/internal/config"
	"builder-code-bot/internal/secret"
)

func TestIsRetryable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "bad connection", err: driver.ErrBadConn, want: true},
		{name: "invalid mysql connection", err: drivermysql.ErrInvalidConn, want: true},
		{name: "wrapped bad connection", err: fmt.Errorf("query: %w", driver.ErrBadConn), want: true},
		{name: "EOF", err: io.EOF, want: true},
		{name: "unexpected EOF", err: io.ErrUnexpectedEOF, want: true},
		{name: "connection refused", err: syscall.ECONNREFUSED, want: true},
		{name: "connection reset", err: syscall.ECONNRESET, want: true},
		{name: "broken pipe", err: syscall.EPIPE, want: true},
		{name: "network unreachable", err: syscall.ENETUNREACH, want: true},
		{name: "host unreachable", err: syscall.EHOSTUNREACH, want: true},
		{name: "network error", err: &net.DNSError{Err: "temporary failure", IsTemporary: true}, want: true},
		{name: "permanent DNS error", err: &net.DNSError{Err: "no such host", IsNotFound: true}, want: false},
		{name: "server shutdown", err: &drivermysql.MySQLError{Number: 1053}, want: true},
		{name: "lock wait timeout", err: &drivermysql.MySQLError{Number: 1205}, want: true},
		{name: "deadlock", err: &drivermysql.MySQLError{Number: 1213}, want: true},
		{name: "connection refused mysql", err: &drivermysql.MySQLError{Number: 2002}, want: true},
		{name: "cannot connect mysql", err: &drivermysql.MySQLError{Number: 2003}, want: true},
		{name: "server gone away", err: &drivermysql.MySQLError{Number: 2006}, want: true},
		{name: "lost connection", err: &drivermysql.MySQLError{Number: 2013}, want: true},
		{name: "access denied", err: &drivermysql.MySQLError{Number: 1044}, want: false},
		{name: "authentication failed", err: &drivermysql.MySQLError{Number: 1045}, want: false},
		{name: "unknown database", err: &drivermysql.MySQLError{Number: 1049}, want: false},
		{name: "unknown column", err: &drivermysql.MySQLError{Number: 1054}, want: false},
		{name: "table missing", err: &drivermysql.MySQLError{Number: 1146}, want: false},
		{name: "context canceled", err: context.Canceled, want: false},
		{name: "context deadline exceeded", err: context.DeadlineExceeded, want: false},
		{name: "ordinary error", err: errors.New("invalid scan destination"), want: false},
		{name: "nil", err: nil, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := IsRetryable(test.err); got != test.want {
				t.Fatalf("IsRetryable(%v) = %t, want %t", test.err, got, test.want)
			}
		})
	}
}

func TestOpenCreatesPoolWithoutConnecting(t *testing.T) {
	t.Parallel()

	db, err := Open(config.MySQLConfig{
		Host:     "127.0.0.1",
		Port:     1,
		Database: "funding/name",
		User:     "funding-user",
		Password: secret.NewString("p@ss/word"),
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if got := db.Stats().MaxOpenConnections; got != maxOpenConns {
		t.Fatalf("MaxOpenConnections = %d, want %d", got, maxOpenConns)
	}
}
