package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"builder-code-bot/internal/funding"
)

const (
	completeBatchSize = 500

	pointsRebateListPendingSQL = `SELECT id, period_start_at, amount
FROM trasia_points_rebate_funding_record
WHERE status = 0
ORDER BY period_start_at, id`

	pointsRebateUpdatePrefix = `UPDATE trasia_points_rebate_funding_record
SET status = 1
WHERE status = 0 AND id IN`

	pointsRebateStatusPrefix = `SELECT id, status
FROM trasia_points_rebate_funding_record
WHERE id IN`

	referralListPendingSQL = `SELECT id, period_start_at, amount
FROM trasia_referral_funding_record
WHERE cycle_type IN (1, 2) AND status = 0
ORDER BY period_start_at, id`

	referralUpdatePrefix = `UPDATE trasia_referral_funding_record
SET status = 1
WHERE cycle_type IN (1, 2) AND status = 0 AND id IN`

	referralStatusPrefix = `SELECT id, status
FROM trasia_referral_funding_record
WHERE cycle_type IN (1, 2) AND id IN`
)

type repositoryQueries struct {
	listPendingSQL    string
	updatePrefix      string
	statusPrefix      string
	listOperation     string
	completeOperation string
}

var (
	pointsRebateQueries = repositoryQueries{
		listPendingSQL:    pointsRebateListPendingSQL,
		updatePrefix:      pointsRebateUpdatePrefix,
		statusPrefix:      pointsRebateStatusPrefix,
		listOperation:     "list_pending",
		completeOperation: "complete_funding_records",
	}
	referralQueries = repositoryQueries{
		listPendingSQL:    referralListPendingSQL,
		updatePrefix:      referralUpdatePrefix,
		statusPrefix:      referralStatusPrefix,
		listOperation:     "list_pending_referral",
		completeOperation: "complete_referral_funding_records",
	}
)

type Repository struct {
	db      *sql.DB
	retryer Retryer
	queries repositoryQueries
}

// NewRepository preserves the original Points Rebate repository constructor.
// New code that assembles both payout targets should use the two explicit
// constructors below.
func NewRepository(db *sql.DB, retryer Retryer) *Repository {
	return NewPointsRebateRepository(db, retryer)
}

func NewPointsRebateRepository(db *sql.DB, retryer Retryer) *Repository {
	return newRepository(db, retryer, pointsRebateQueries)
}

func NewReferralRepository(db *sql.DB, retryer Retryer) *Repository {
	return newRepository(db, retryer, referralQueries)
}

func newRepository(db *sql.DB, retryer Retryer, queries repositoryQueries) *Repository {
	return &Repository{db: db, retryer: retryer, queries: queries}
}

func (r *Repository) ListPending(ctx context.Context) ([]funding.Record, error) {
	var records []funding.Record
	err := r.retryer.Do(ctx, r.queries.listOperation, func(ctx context.Context) (attemptErr error) {
		rows, err := r.db.QueryContext(ctx, r.queries.listPendingSQL)
		if err != nil {
			return err
		}
		defer func() {
			if closeErr := rows.Close(); closeErr != nil {
				attemptErr = appendDiagnosticError(
					attemptErr, fmt.Errorf("close pending funding rows: %w", closeErr),
				)
			}
		}()

		attemptRecords := make([]funding.Record, 0)
		for rows.Next() {
			var record funding.Record
			if err := rows.Scan(&record.ID, &record.PeriodStartAt, &record.Amount); err != nil {
				return fmt.Errorf("scan pending funding record: %w", err)
			}
			attemptRecords = append(attemptRecords, record)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		records = attemptRecords
		return nil
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}

func (r *Repository) Complete(ctx context.Context, ids []uint64) error {
	manifestIDs := uniqueIDs(ids)
	if len(manifestIDs) == 0 {
		return nil
	}

	remaining := manifestIDs
	var (
		resolveCommit bool
		commitErr     error
	)
	return r.retryer.Do(ctx, r.queries.completeOperation, func(ctx context.Context) error {
		if resolveCommit {
			zeroIDs, err := readStatuses(ctx, r.db, r.queries, manifestIDs)
			if err != nil {
				return err
			}
			if len(zeroIDs) == 0 {
				return nil
			}
			if !IsRetryable(commitErr) {
				return commitErr
			}
			remaining = zeroIDs
			resolveCommit = false
		}

		committed, err := completeTransaction(ctx, r.db, r.queries, remaining)
		if err == nil {
			return nil
		}
		if !committed {
			return err
		}

		// COMMIT may have reached the server even when its response was lost.
		// Preserve that ambiguity until a fresh pooled connection verifies every
		// manifest ID; only confirmed status-zero rows may be replayed.
		r.retryer.Diagnostic(r.queries.completeOperation, "commit_response_failed", err)
		resolveCommit = true
		commitErr = err
		zeroIDs, statusErr := readStatuses(ctx, r.db, r.queries, manifestIDs)
		if statusErr != nil {
			return statusErr
		}
		if len(zeroIDs) == 0 {
			return nil
		}
		if !IsRetryable(commitErr) {
			return commitErr
		}
		remaining = zeroIDs
		resolveCommit = false
		return commitErr
	})
}

func completeTransaction(
	ctx context.Context,
	db *sql.DB,
	queries repositoryQueries,
	ids []uint64,
) (commitAttempted bool, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = appendDiagnosticError(err, fmt.Errorf("rollback funding transaction: %w", rollbackErr))
		}
	}()

	forEachIDBatch(ids, func(batch []uint64) bool {
		result, execErr := tx.ExecContext(
			ctx,
			queries.updatePrefix+" ("+placeholders(len(batch))+")",
			uint64Args(batch)...,
		)
		if execErr != nil {
			err = execErr
			return false
		}
		affected, affectedErr := result.RowsAffected()
		if affectedErr != nil {
			err = fmt.Errorf("read affected funding rows: %w", affectedErr)
			return false
		}
		if affected < 0 || affected > int64(len(batch)) {
			err = fmt.Errorf("update funding records affected %d rows for %d manifest IDs", affected, len(batch))
			return false
		}
		return true
	})
	if err != nil {
		return false, err
	}
	zeroIDs, err := readStatuses(ctx, tx, queries, ids)
	if err != nil {
		return false, err
	}
	if len(zeroIDs) != 0 {
		return false, fmt.Errorf("%d funding records remain at status 0 after update", len(zeroIDs))
	}
	if err := tx.Commit(); err != nil {
		return true, err
	}
	return true, nil
}

type statusQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// readStatuses returns only status-zero IDs. Missing rows and statuses other
// than zero or one are permanent integrity errors.
func readStatuses(
	ctx context.Context,
	queryer statusQueryer,
	queries repositoryQueries,
	ids []uint64,
) ([]uint64, error) {
	zeroIDs := make([]uint64, 0)
	var readErr error
	forEachIDBatch(ids, func(batch []uint64) bool {
		rows, err := queryer.QueryContext(
			ctx,
			queries.statusPrefix+" ("+placeholders(len(batch))+") ORDER BY id",
			uint64Args(batch)...,
		)
		if err != nil {
			readErr = err
			return false
		}

		statuses := make(map[uint64]int, len(batch))
		for rows.Next() {
			var (
				id     uint64
				status int
			)
			if err := rows.Scan(&id, &status); err != nil {
				readErr = fmt.Errorf("scan funding record status: %w", err)
				if closeErr := rows.Close(); closeErr != nil {
					readErr = appendDiagnosticError(
						readErr, fmt.Errorf("close funding status rows: %w", closeErr),
					)
				}
				return false
			}
			if _, duplicate := statuses[id]; duplicate {
				readErr = fmt.Errorf("funding record %d returned more than once", id)
				if closeErr := rows.Close(); closeErr != nil {
					readErr = appendDiagnosticError(
						readErr, fmt.Errorf("close funding status rows: %w", closeErr),
					)
				}
				return false
			}
			statuses[id] = status
		}
		if err := rows.Err(); err != nil {
			readErr = err
			if closeErr := rows.Close(); closeErr != nil {
				readErr = appendDiagnosticError(
					readErr, fmt.Errorf("close funding status rows: %w", closeErr),
				)
			}
			return false
		}
		if err := rows.Close(); err != nil {
			readErr = err
			return false
		}

		for _, id := range batch {
			status, ok := statuses[id]
			if !ok {
				readErr = fmt.Errorf("funding record %d is missing", id)
				return false
			}
			switch status {
			case 0:
				zeroIDs = append(zeroIDs, id)
			case 1:
			default:
				readErr = fmt.Errorf("funding record %d has unexpected status %d", id, status)
				return false
			}
		}
		return true
	})
	if readErr != nil {
		return nil, readErr
	}
	return zeroIDs, nil
}

type errorWithDiagnostic struct {
	primary    error
	diagnostic error
}

func (e *errorWithDiagnostic) Error() string {
	return e.primary.Error() + "\n" + e.diagnostic.Error()
}

func (e *errorWithDiagnostic) Unwrap() error { return e.primary }

func appendDiagnosticError(primary, diagnostic error) error {
	if primary == nil {
		return diagnostic
	}
	if diagnostic == nil {
		return primary
	}
	return &errorWithDiagnostic{primary: primary, diagnostic: diagnostic}
}

func forEachIDBatch(ids []uint64, fn func([]uint64) bool) {
	for start := 0; start < len(ids); start += completeBatchSize {
		end := min(start+completeBatchSize, len(ids))
		if !fn(ids[start:end]) {
			return
		}
	}
}

func placeholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

func uint64Args(ids []uint64) []any {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return args
}

func uniqueIDs(ids []uint64) []uint64 {
	seen := make(map[uint64]struct{}, len(ids))
	unique := make([]uint64, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	return unique
}
