package db

import (
	"time"

	"github.com/google/uuid"
)

// SandboxState represents the state of a sandbox.
type SandboxState string

const (
	SandboxStateCreating  SandboxState = "creating"
	SandboxStateWarming   SandboxState = "warming"
	SandboxStateAvailable SandboxState = "available"
	SandboxStateClaimed   SandboxState = "claimed"
	SandboxStateExecuting SandboxState = "executing"
	SandboxStateDeleting  SandboxState = "deleting"
	SandboxStateDeleted   SandboxState = "deleted"
	SandboxStateFailed    SandboxState = "failed"
)

// ExecutionStatus represents the status of an execution.
type ExecutionStatus string

const (
	ExecutionStatusPending    ExecutionStatus = "pending"
	ExecutionStatusSyncingIn  ExecutionStatus = "syncing_in"
	ExecutionStatusExecuting  ExecutionStatus = "executing"
	ExecutionStatusSyncingOut ExecutionStatus = "syncing_out"
	ExecutionStatusCompleted  ExecutionStatus = "completed"
	ExecutionStatusFailed     ExecutionStatus = "failed"
	ExecutionStatusTimeout    ExecutionStatus = "timeout"
)

// Sandbox represents a sandbox in the pool.
type Sandbox struct {
	ID           uuid.UUID    `json:"id"`
	DaytonaID    string       `json:"daytona_id"`
	State        SandboxState `json:"state"`
	ClaimedBy    *uuid.UUID   `json:"claimed_by,omitempty"`
	ClaimedAt    *time.Time   `json:"claimed_at,omitempty"`
	CreatedAt    time.Time    `json:"created_at"`
	LastUsedAt   *time.Time   `json:"last_used_at,omitempty"`
	ErrorMessage *string      `json:"error_message,omitempty"`
	Version      int          `json:"version"`
}

// Execution represents a code execution request.
type Execution struct {
	ID              uuid.UUID       `json:"id"`
	UserID          string          `json:"user_id"`
	ChatID          string          `json:"chat_id"`
	SandboxID       *uuid.UUID      `json:"sandbox_id,omitempty"`
	Language        string          `json:"language"`
	Path            string          `json:"path"`
	Code            string          `json:"code"`
	Status          ExecutionStatus `json:"status"`
	Stdout          *string         `json:"stdout,omitempty"`
	Stderr          *string         `json:"stderr,omitempty"`
	ExitCode        *int            `json:"exit_code,omitempty"`
	ExecutionTimeMs *int            `json:"execution_time_ms,omitempty"`
	InputFiles      []byte          `json:"input_files,omitempty"`
	OutputFiles     []byte          `json:"output_files,omitempty"`
	TimeoutSeconds  int             `json:"timeout_seconds"`
	CreatedAt       time.Time       `json:"created_at"`
	StartedAt       *time.Time      `json:"started_at,omitempty"`
	CompletedAt     *time.Time      `json:"completed_at,omitempty"`
}

// PoolStats represents pool statistics.
type PoolStats struct {
	Creating  int `json:"creating"`
	Warming   int `json:"warming"`
	Available int `json:"available"`
	Claimed   int `json:"claimed"`
	Executing int `json:"executing"`
	Deleting  int `json:"deleting"`
	Failed    int `json:"failed"`
	Total     int `json:"total"`
}

// FileInfo represents file metadata stored in JSON columns.
type FileInfo struct {
	Path      string `json:"path"`
	MediaType string `json:"media_type"`
	Size      int64  `json:"size"`
}
