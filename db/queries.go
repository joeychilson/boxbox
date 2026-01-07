package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrNoAvailableSandbox is returned when no sandbox is available.
var ErrNoAvailableSandbox = errors.New("no available sandbox")

// ErrNotFound is returned when a record is not found.
var ErrNotFound = errors.New("not found")

// CreateSandbox creates a new sandbox in the pool.
func (db *DB) CreateSandbox(ctx context.Context, daytonaID string) (*Sandbox, error) {
	var s Sandbox
	err := db.pool.QueryRow(ctx, `
		INSERT INTO sandbox_pool (daytona_id, state)
		VALUES ($1, $2)
		RETURNING id, daytona_id, state, claimed_by, claimed_at, created_at, last_used_at, error_message, version
	`, daytonaID, SandboxStateCreating).Scan(
		&s.ID, &s.DaytonaID, &s.State, &s.ClaimedBy, &s.ClaimedAt,
		&s.CreatedAt, &s.LastUsedAt, &s.ErrorMessage, &s.Version,
	)
	if err != nil {
		return nil, fmt.Errorf("inserting sandbox: %w", err)
	}
	return &s, nil
}

// UpdateSandboxState updates a sandbox's state.
func (db *DB) UpdateSandboxState(ctx context.Context, id uuid.UUID, state SandboxState) error {
	_, err := db.pool.Exec(ctx, `
		UPDATE sandbox_pool SET state = $1, version = version + 1 WHERE id = $2
	`, state, id)
	return err
}

// UpdateSandboxStateWithError updates a sandbox's state with an error message.
func (db *DB) UpdateSandboxStateWithError(ctx context.Context, id uuid.UUID, state SandboxState, errMsg string) error {
	_, err := db.pool.Exec(ctx, `
		UPDATE sandbox_pool SET state = $1, error_message = $2, version = version + 1 WHERE id = $3
	`, state, errMsg, id)
	return err
}

// ClaimAvailableSandbox atomically claims an available sandbox.
func (db *DB) ClaimAvailableSandbox(ctx context.Context, executionID uuid.UUID) (*Sandbox, error) {
	var s Sandbox
	err := db.pool.QueryRow(ctx, `
		UPDATE sandbox_pool
		SET state = $1, claimed_by = $2, claimed_at = NOW(), last_used_at = NOW(), version = version + 1
		WHERE id = (
			SELECT id FROM sandbox_pool
			WHERE state = 'available'
			ORDER BY created_at ASC
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING id, daytona_id, state, claimed_by, claimed_at, created_at, last_used_at, error_message, version
	`, SandboxStateClaimed, executionID).Scan(
		&s.ID, &s.DaytonaID, &s.State, &s.ClaimedBy, &s.ClaimedAt,
		&s.CreatedAt, &s.LastUsedAt, &s.ErrorMessage, &s.Version,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoAvailableSandbox
	}
	if err != nil {
		return nil, fmt.Errorf("claiming sandbox: %w", err)
	}
	return &s, nil
}

// GetSandbox gets a sandbox by ID.
func (db *DB) GetSandbox(ctx context.Context, id uuid.UUID) (*Sandbox, error) {
	var s Sandbox
	err := db.pool.QueryRow(ctx, `
		SELECT id, daytona_id, state, claimed_by, claimed_at, created_at, last_used_at, error_message, version
		FROM sandbox_pool WHERE id = $1
	`, id).Scan(
		&s.ID, &s.DaytonaID, &s.State, &s.ClaimedBy, &s.ClaimedAt,
		&s.CreatedAt, &s.LastUsedAt, &s.ErrorMessage, &s.Version,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("getting sandbox: %w", err)
	}
	return &s, nil
}

// DeleteSandbox marks a sandbox as deleted.
func (db *DB) DeleteSandbox(ctx context.Context, id uuid.UUID) error {
	_, err := db.pool.Exec(ctx, `
		UPDATE sandbox_pool SET state = $1, version = version + 1 WHERE id = $2
	`, SandboxStateDeleted, id)
	return err
}

// GetPoolStats returns pool statistics.
func (db *DB) GetPoolStats(ctx context.Context) (*PoolStats, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT state, COUNT(*) FROM sandbox_pool WHERE state != 'deleted' GROUP BY state
	`)
	if err != nil {
		return nil, fmt.Errorf("querying pool stats: %w", err)
	}
	defer rows.Close()

	stats := &PoolStats{}
	for rows.Next() {
		var state string
		var count int
		if err := rows.Scan(&state, &count); err != nil {
			return nil, fmt.Errorf("scanning row: %w", err)
		}
		switch SandboxState(state) {
		case SandboxStateCreating:
			stats.Creating = count
		case SandboxStateWarming:
			stats.Warming = count
		case SandboxStateAvailable:
			stats.Available = count
		case SandboxStateClaimed:
			stats.Claimed = count
		case SandboxStateExecuting:
			stats.Executing = count
		case SandboxStateDeleting:
			stats.Deleting = count
		case SandboxStateFailed:
			stats.Failed = count
		}
		stats.Total += count
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating rows: %w", err)
	}

	return stats, nil
}

// GetAvailableCount returns the count of available sandboxes.
func (db *DB) GetAvailableCount(ctx context.Context) (int, error) {
	var count int
	err := db.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM sandbox_pool WHERE state = 'available'
	`).Scan(&count)
	return count, err
}

// GetStaleSandboxes returns sandboxes stuck in transient states.
func (db *DB) GetStaleSandboxes(ctx context.Context, maxAge time.Duration) ([]Sandbox, error) {
	cutoff := time.Now().Add(-maxAge)
	rows, err := db.pool.Query(ctx, `
		SELECT id, daytona_id, state, claimed_by, claimed_at, created_at, last_used_at, error_message, version
		FROM sandbox_pool
		WHERE state IN ('creating', 'warming', 'claimed') AND created_at < $1
	`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("querying stale sandboxes: %w", err)
	}
	defer rows.Close()

	var sandboxes []Sandbox
	for rows.Next() {
		var s Sandbox
		if err := rows.Scan(
			&s.ID, &s.DaytonaID, &s.State, &s.ClaimedBy, &s.ClaimedAt,
			&s.CreatedAt, &s.LastUsedAt, &s.ErrorMessage, &s.Version,
		); err != nil {
			return nil, fmt.Errorf("scanning row: %w", err)
		}
		sandboxes = append(sandboxes, s)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating rows: %w", err)
	}

	return sandboxes, nil
}

// GetOldAvailableSandboxes returns available sandboxes older than maxAge.
func (db *DB) GetOldAvailableSandboxes(ctx context.Context, maxAge time.Duration, limit int) ([]Sandbox, error) {
	cutoff := time.Now().Add(-maxAge)
	rows, err := db.pool.Query(ctx, `
		SELECT id, daytona_id, state, claimed_by, claimed_at, created_at, last_used_at, error_message, version
		FROM sandbox_pool
		WHERE state = 'available' AND created_at < $1
		ORDER BY created_at ASC
		LIMIT $2
	`, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("querying old sandboxes: %w", err)
	}
	defer rows.Close()

	var sandboxes []Sandbox
	for rows.Next() {
		var s Sandbox
		if err := rows.Scan(
			&s.ID, &s.DaytonaID, &s.State, &s.ClaimedBy, &s.ClaimedAt,
			&s.CreatedAt, &s.LastUsedAt, &s.ErrorMessage, &s.Version,
		); err != nil {
			return nil, fmt.Errorf("scanning row: %w", err)
		}
		sandboxes = append(sandboxes, s)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating rows: %w", err)
	}

	return sandboxes, nil
}

// CreateExecution creates a new execution record.
func (db *DB) CreateExecution(ctx context.Context, userID, chatID, language, path, code string, timeoutSeconds int) (*Execution, error) {
	var e Execution
	err := db.pool.QueryRow(ctx, `
		INSERT INTO executions (user_id, chat_id, language, path, code, status, timeout_seconds)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, user_id, chat_id, sandbox_id, language, path, code, status, stdout, stderr, exit_code, execution_time_ms, input_files, output_files, timeout_seconds, created_at, started_at, completed_at
	`, userID, chatID, language, path, code, ExecutionStatusPending, timeoutSeconds).Scan(
		&e.ID, &e.UserID, &e.ChatID, &e.SandboxID, &e.Language, &e.Path, &e.Code, &e.Status,
		&e.Stdout, &e.Stderr, &e.ExitCode, &e.ExecutionTimeMs, &e.InputFiles, &e.OutputFiles,
		&e.TimeoutSeconds, &e.CreatedAt, &e.StartedAt, &e.CompletedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("inserting execution: %w", err)
	}
	return &e, nil
}

// UpdateExecutionSandbox links an execution to a sandbox.
func (db *DB) UpdateExecutionSandbox(ctx context.Context, executionID, sandboxID uuid.UUID) error {
	_, err := db.pool.Exec(ctx, `
		UPDATE executions SET sandbox_id = $1 WHERE id = $2
	`, sandboxID, executionID)
	return err
}

// UpdateExecutionStatus updates an execution's status.
func (db *DB) UpdateExecutionStatus(ctx context.Context, id uuid.UUID, status ExecutionStatus) error {
	var query string
	if status == ExecutionStatusExecuting {
		query = `UPDATE executions SET status = $1, started_at = NOW() WHERE id = $2`
	} else {
		query = `UPDATE executions SET status = $1 WHERE id = $2`
	}
	_, err := db.pool.Exec(ctx, query, status, id)
	return err
}

// UpdateExecutionInputFiles updates the input files for an execution.
func (db *DB) UpdateExecutionInputFiles(ctx context.Context, id uuid.UUID, inputFiles []byte) error {
	_, err := db.pool.Exec(ctx, `
		UPDATE executions SET input_files = $1 WHERE id = $2
	`, inputFiles, id)
	return err
}

// CompleteExecution updates an execution with results.
func (db *DB) CompleteExecution(ctx context.Context, id uuid.UUID, status ExecutionStatus, exitCode int, stdout, stderr string, executionTimeMs int, outputFiles []byte) error {
	_, err := db.pool.Exec(ctx, `
		UPDATE executions
		SET status = $1, exit_code = $2, stdout = $3, stderr = $4, execution_time_ms = $5, output_files = $6, completed_at = NOW()
		WHERE id = $7
	`, status, exitCode, stdout, stderr, executionTimeMs, outputFiles, id)
	return err
}

// GetExecution gets an execution by ID.
func (db *DB) GetExecution(ctx context.Context, id uuid.UUID) (*Execution, error) {
	var e Execution
	err := db.pool.QueryRow(ctx, `
		SELECT id, user_id, chat_id, sandbox_id, language, path, code, status, stdout, stderr, exit_code, execution_time_ms, input_files, output_files, timeout_seconds, created_at, started_at, completed_at
		FROM executions WHERE id = $1
	`, id).Scan(
		&e.ID, &e.UserID, &e.ChatID, &e.SandboxID, &e.Language, &e.Path, &e.Code, &e.Status,
		&e.Stdout, &e.Stderr, &e.ExitCode, &e.ExecutionTimeMs, &e.InputFiles, &e.OutputFiles,
		&e.TimeoutSeconds, &e.CreatedAt, &e.StartedAt, &e.CompletedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("getting execution: %w", err)
	}
	return &e, nil
}

// GetOrphanedSandboxes returns sandboxes in non-terminal states for cleanup.
func (db *DB) GetOrphanedSandboxes(ctx context.Context) ([]Sandbox, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT id, daytona_id, state, claimed_by, claimed_at, created_at, last_used_at, error_message, version
		FROM sandbox_pool
		WHERE state NOT IN ('deleted', 'available')
	`)
	if err != nil {
		return nil, fmt.Errorf("querying orphaned sandboxes: %w", err)
	}
	defer rows.Close()

	var sandboxes []Sandbox
	for rows.Next() {
		var s Sandbox
		if err := rows.Scan(
			&s.ID, &s.DaytonaID, &s.State, &s.ClaimedBy, &s.ClaimedAt,
			&s.CreatedAt, &s.LastUsedAt, &s.ErrorMessage, &s.Version,
		); err != nil {
			return nil, fmt.Errorf("scanning row: %w", err)
		}
		sandboxes = append(sandboxes, s)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating rows: %w", err)
	}

	return sandboxes, nil
}

// GetAllDaytonaIDs returns all Daytona IDs that are not deleted.
func (db *DB) GetAllDaytonaIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT daytona_id FROM sandbox_pool WHERE state != 'deleted'
	`)
	if err != nil {
		return nil, fmt.Errorf("querying daytona IDs: %w", err)
	}
	defer rows.Close()

	ids := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning row: %w", err)
		}
		ids[id] = true
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating rows: %w", err)
	}

	return ids, nil
}

// GetFailedSandboxes returns sandboxes in failed state.
func (db *DB) GetFailedSandboxes(ctx context.Context) ([]Sandbox, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT id, daytona_id, state, claimed_by, claimed_at, created_at, last_used_at, error_message, version
		FROM sandbox_pool
		WHERE state = 'failed'
	`)
	if err != nil {
		return nil, fmt.Errorf("querying failed sandboxes: %w", err)
	}
	defer rows.Close()

	var sandboxes []Sandbox
	for rows.Next() {
		var s Sandbox
		if err := rows.Scan(
			&s.ID, &s.DaytonaID, &s.State, &s.ClaimedBy, &s.ClaimedAt,
			&s.CreatedAt, &s.LastUsedAt, &s.ErrorMessage, &s.Version,
		); err != nil {
			return nil, fmt.Errorf("scanning row: %w", err)
		}
		sandboxes = append(sandboxes, s)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating rows: %w", err)
	}

	return sandboxes, nil
}

// GetAvailableSandboxes returns up to limit available sandboxes for health checking.
func (db *DB) GetAvailableSandboxes(ctx context.Context, limit int) ([]Sandbox, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT id, daytona_id, state, claimed_by, claimed_at, created_at, last_used_at, error_message, version
		FROM sandbox_pool
		WHERE state = 'available'
		ORDER BY created_at ASC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("querying available sandboxes: %w", err)
	}
	defer rows.Close()

	var sandboxes []Sandbox
	for rows.Next() {
		var s Sandbox
		if err := rows.Scan(
			&s.ID, &s.DaytonaID, &s.State, &s.ClaimedBy, &s.ClaimedAt,
			&s.CreatedAt, &s.LastUsedAt, &s.ErrorMessage, &s.Version,
		); err != nil {
			return nil, fmt.Errorf("scanning row: %w", err)
		}
		sandboxes = append(sandboxes, s)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating rows: %w", err)
	}

	return sandboxes, nil
}
