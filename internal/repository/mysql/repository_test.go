package mysql

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestPointsRebateRepositoryListPendingPreservesExactDecimalText(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectQuery(regexp.QuoteMeta(pointsRebateListPendingSQL)).WillReturnRows(
		sqlmock.NewRows([]string{"id", "period_start_at", "amount"}).
			AddRow(2, 100, []byte("0.000000000000000001")).
			AddRow(9, 101, []byte("123456789012345678901234567890.123456789012345678")),
	)

	repository := NewPointsRebateRepository(db, immediateRetryer())
	records, err := repository.ListPending(context.Background())
	if err != nil {
		t.Fatalf("ListPending() error = %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("record count = %d, want 2", len(records))
	}
	if records[0].Amount != "0.000000000000000001" {
		t.Errorf("first amount = %q", records[0].Amount)
	}
	if records[1].Amount != "123456789012345678901234567890.123456789012345678" {
		t.Errorf("second amount = %q", records[1].Amount)
	}
	if records[0].ID != 2 || records[1].ID != 9 {
		t.Errorf("record order = [%d, %d], want [2, 9]", records[0].ID, records[1].ID)
	}
	assertSQLMockExpectations(t, mock)
}

func TestReferralRepositoryListPendingUsesFixedCycleTypesAndPreservesRecords(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectQuery(regexp.QuoteMeta(referralListPendingSQL)).WillReturnRows(
		sqlmock.NewRows([]string{"id", "period_start_at", "amount"}).
			AddRow(4, 200, []byte("0.000000000000000001")).
			AddRow(8, 201, []byte("9876543210.123456789012345678")),
	)

	repository := NewReferralRepository(db, immediateRetryer())
	records, err := repository.ListPending(context.Background())
	if err != nil {
		t.Fatalf("ListPending() error = %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("record count = %d, want 2", len(records))
	}
	if records[0].ID != 4 || records[0].PeriodStartAt != 200 ||
		records[0].Amount != "0.000000000000000001" {
		t.Errorf("first record = %#v", records[0])
	}
	if records[1].ID != 8 || records[1].PeriodStartAt != 201 ||
		records[1].Amount != "9876543210.123456789012345678" {
		t.Errorf("second record = %#v", records[1])
	}
	assertSQLMockExpectations(t, mock)
}

func TestRepositoryListPendingPreservesScanAndCloseErrors(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	rows := sqlmock.NewRows([]string{"id", "period_start_at", "amount"}).
		AddRow("invalid-id", 100, "1").
		AddRow(2, 101, "2").
		CloseError(driver.ErrBadConn)
	mock.ExpectQuery(regexp.QuoteMeta(pointsRebateListPendingSQL)).WillReturnRows(rows)

	_, err = NewPointsRebateRepository(db, immediateRetryer()).ListPending(context.Background())
	if err == nil || !strings.Contains(err.Error(), "scan pending funding record") ||
		!strings.Contains(err.Error(), driver.ErrBadConn.Error()) || IsRetryable(err) {
		t.Fatalf("ListPending() error = %v, want scan and close causes", err)
	}
	assertSQLMockExpectations(t, mock)
}

func TestPointsRebateRepositoryCompleteUpdatesOnlyManifestIDsAndVerifiesStatus(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(pointsRebateUpdatePrefix+" (?,?)")).
		WithArgs(uint64(3), uint64(7)).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectQuery(regexp.QuoteMeta(pointsRebateStatusPrefix+" (?,?) ORDER BY id")).
		WithArgs(uint64(3), uint64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(3, 1).AddRow(7, 1))
	mock.ExpectCommit()

	repository := NewPointsRebateRepository(db, immediateRetryer())
	if err := repository.Complete(context.Background(), []uint64{3, 7}); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	assertSQLMockExpectations(t, mock)
}

func TestReferralRepositoryCompleteKeepsFixedCycleTypesInUpdateAndStatusRead(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(referralUpdatePrefix+" (?,?)")).
		WithArgs(uint64(3), uint64(7)).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectQuery(regexp.QuoteMeta(referralStatusPrefix+" (?,?) ORDER BY id")).
		WithArgs(uint64(3), uint64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(3, 1).AddRow(7, 1))
	mock.ExpectCommit()

	repository := NewReferralRepository(db, immediateRetryer())
	if err := repository.Complete(context.Background(), []uint64{3, 7}); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	assertSQLMockExpectations(t, mock)
}

func TestReferralRepositoryCompleteRejectsRecordOutsideFixedCycleTypes(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(referralUpdatePrefix + " (?)")).
		WithArgs(uint64(3)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(referralStatusPrefix + " (?) ORDER BY id")).
		WithArgs(uint64(3)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}))
	mock.ExpectRollback()

	repository := NewReferralRepository(db, immediateRetryer())
	err = repository.Complete(context.Background(), []uint64{3})
	if err == nil || IsRetryable(err) {
		t.Fatalf("Complete() error = %v, want permanent out-of-scope record error", err)
	}
	assertSQLMockExpectations(t, mock)
}

func TestRepositoryCompleteRejectsMissingRecord(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(pointsRebateUpdatePrefix+" (?,?)")).
		WithArgs(uint64(3), uint64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(pointsRebateStatusPrefix+" (?,?) ORDER BY id")).
		WithArgs(uint64(3), uint64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(3, 1))
	mock.ExpectRollback()

	repository := NewRepository(db, immediateRetryer())
	err = repository.Complete(context.Background(), []uint64{3, 7})
	if err == nil || IsRetryable(err) {
		t.Fatalf("Complete() error = %v, want permanent missing-record error", err)
	}
	assertSQLMockExpectations(t, mock)
}

func TestRepositoryCompleteRejectsUnexpectedAffectedRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(pointsRebateUpdatePrefix + " (?)")).
		WithArgs(uint64(3)).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectRollback()

	repository := NewRepository(db, immediateRetryer())
	err = repository.Complete(context.Background(), []uint64{3})
	if err == nil || IsRetryable(err) {
		t.Fatalf("Complete() error = %v, want permanent affected-rows error", err)
	}
	assertSQLMockExpectations(t, mock)
}

func TestRepositoryCompleteRejectsRecordStillPendingAfterUpdate(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(pointsRebateUpdatePrefix + " (?)")).
		WithArgs(uint64(3)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(pointsRebateStatusPrefix + " (?) ORDER BY id")).
		WithArgs(uint64(3)).
		WillReturnRows(statusRows([]uint64{3}, 0))
	mock.ExpectRollback()

	repository := NewRepository(db, immediateRetryer())
	err = repository.Complete(context.Background(), []uint64{3})
	if err == nil || IsRetryable(err) {
		t.Fatalf("Complete() error = %v, want permanent incomplete-status error", err)
	}
	assertSQLMockExpectations(t, mock)
}

func TestRepositoryCompleteBatchesFiveHundredIDsInOneTransaction(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ids := make([]uint64, 501)
	for i := range ids {
		ids[i] = uint64(i + 1)
	}
	firstArgs := driverArgs(ids[:500])
	secondArgs := driverArgs(ids[500:])

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(pointsRebateUpdatePrefix + " (" + placeholders(500) + ")")).
		WithArgs(firstArgs...).
		WillReturnResult(sqlmock.NewResult(0, 500))
	mock.ExpectExec(regexp.QuoteMeta(pointsRebateUpdatePrefix + " (?)")).
		WithArgs(secondArgs...).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(pointsRebateStatusPrefix + " (" + placeholders(500) + ") ORDER BY id")).
		WithArgs(firstArgs...).
		WillReturnRows(statusRows(ids[:500], 1))
	mock.ExpectQuery(regexp.QuoteMeta(pointsRebateStatusPrefix + " (?) ORDER BY id")).
		WithArgs(secondArgs...).
		WillReturnRows(statusRows(ids[500:], 1))
	mock.ExpectCommit()

	repository := NewRepository(db, immediateRetryer())
	if err := repository.Complete(context.Background(), ids); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	assertSQLMockExpectations(t, mock)
}

func TestRepositoryCompleteResolvesAmbiguousCommitAsSuccess(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(pointsRebateUpdatePrefix+" (?,?)")).
		WithArgs(uint64(3), uint64(7)).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectQuery(regexp.QuoteMeta(pointsRebateStatusPrefix+" (?,?) ORDER BY id")).
		WithArgs(uint64(3), uint64(7)).
		WillReturnRows(statusRows([]uint64{3, 7}, 1))
	mock.ExpectCommit().WillReturnError(io.EOF)
	mock.ExpectQuery(regexp.QuoteMeta(pointsRebateStatusPrefix+" (?,?) ORDER BY id")).
		WithArgs(uint64(3), uint64(7)).
		WillReturnRows(statusRows([]uint64{3, 7}, 1))

	observer := &recordingRetryObserver{}
	retryer := immediateRetryer()
	retryer.observer = observer
	repository := NewRepository(db, retryer)
	if err := repository.Complete(context.Background(), []uint64{3, 7}); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if len(observer.events) != 1 || observer.events[0].kind != "diagnostic" ||
		observer.events[0].operation != "complete_funding_records" ||
		observer.events[0].stage != "commit_response_failed" ||
		!errors.Is(observer.events[0].err, io.EOF) {
		t.Fatalf("commit diagnostics = %#v", observer.events)
	}
	assertSQLMockExpectations(t, mock)
}

func TestRepositoryCompleteRetriesOnlyZeroStatusAfterAmbiguousCommit(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(pointsRebateUpdatePrefix+" (?,?)")).
		WithArgs(uint64(3), uint64(7)).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectQuery(regexp.QuoteMeta(pointsRebateStatusPrefix+" (?,?) ORDER BY id")).
		WithArgs(uint64(3), uint64(7)).
		WillReturnRows(statusRows([]uint64{3, 7}, 1))
	mock.ExpectCommit().WillReturnError(io.EOF)
	mock.ExpectQuery(regexp.QuoteMeta(pointsRebateStatusPrefix+" (?,?) ORDER BY id")).
		WithArgs(uint64(3), uint64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(3, 1).AddRow(7, 0))
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(pointsRebateUpdatePrefix + " (?)")).
		WithArgs(uint64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(pointsRebateStatusPrefix + " (?) ORDER BY id")).
		WithArgs(uint64(7)).
		WillReturnRows(statusRows([]uint64{7}, 1))
	mock.ExpectCommit()

	repository := NewRepository(db, immediateRetryer())
	if err := repository.Complete(context.Background(), []uint64{3, 7}); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	assertSQLMockExpectations(t, mock)
}

func TestReferralRepositoryCompleteRetriesOnlyScopedZeroStatusAfterAmbiguousCommit(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(referralUpdatePrefix+" (?,?)")).
		WithArgs(uint64(3), uint64(7)).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectQuery(regexp.QuoteMeta(referralStatusPrefix+" (?,?) ORDER BY id")).
		WithArgs(uint64(3), uint64(7)).
		WillReturnRows(statusRows([]uint64{3, 7}, 1))
	mock.ExpectCommit().WillReturnError(io.EOF)
	mock.ExpectQuery(regexp.QuoteMeta(referralStatusPrefix+" (?,?) ORDER BY id")).
		WithArgs(uint64(3), uint64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(3, 1).AddRow(7, 0))
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(referralUpdatePrefix + " (?)")).
		WithArgs(uint64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(referralStatusPrefix + " (?) ORDER BY id")).
		WithArgs(uint64(7)).
		WillReturnRows(statusRows([]uint64{7}, 1))
	mock.ExpectCommit()

	repository := NewReferralRepository(db, immediateRetryer())
	if err := repository.Complete(context.Background(), []uint64{3, 7}); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	assertSQLMockExpectations(t, mock)
}

func TestNewRepositoryRemainsPointsRebateCompatibilityConstructor(t *testing.T) {
	repository := NewRepository(nil, Retryer{})
	if repository.queries != pointsRebateQueries {
		t.Fatalf("NewRepository() queries = %#v, want Points Rebate", repository.queries)
	}
}

func immediateRetryer() Retryer {
	return Retryer{
		now: time.Now,
		sleep: func(context.Context, time.Duration) error {
			return nil
		},
		jitter: func(delay time.Duration) time.Duration { return delay },
	}
}

func driverArgs(ids []uint64) []driver.Value {
	args := make([]driver.Value, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return args
}

func statusRows(ids []uint64, status int) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{"id", "status"})
	for _, id := range ids {
		rows.AddRow(id, status)
	}
	return rows
}

func assertSQLMockExpectations(t *testing.T, mock sqlmock.Sqlmock) {
	t.Helper()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}
