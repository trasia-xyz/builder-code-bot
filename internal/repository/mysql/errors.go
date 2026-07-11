package mysql

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"net"
	"syscall"

	drivermysql "github.com/go-sql-driver/mysql"
)

var retryableMySQLErrors = map[uint16]struct{}{
	1040: {}, // Too many connections.
	1053: {}, // Server shutdown in progress.
	1205: {}, // Lock wait timeout.
	1213: {}, // Deadlock.
	2002: {}, // Cannot connect through socket.
	2003: {}, // Cannot connect to server.
	2006: {}, // Server has gone away.
	2013: {}, // Lost connection during query.
}

// IsRetryable reports whether an error is known to be transient. Unknown
// errors are deliberately permanent so SQL, scan, and data errors fail fast.
func IsRetryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, drivermysql.ErrInvalidConn) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH) {
		return true
	}

	var mysqlErr *drivermysql.MySQLError
	if errors.As(err, &mysqlErr) {
		_, ok := retryableMySQLErrors[mysqlErr.Number]
		return ok
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsTemporary && !dnsErr.IsNotFound {
		return true
	}

	var timeoutErr interface{ Timeout() bool }
	return errors.As(err, &timeoutErr) && timeoutErr.Timeout()
}
