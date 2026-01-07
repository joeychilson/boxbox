-- Pool of sandboxes
CREATE TABLE IF NOT EXISTS sandbox_pool (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    daytona_id VARCHAR(255) UNIQUE NOT NULL,
    state VARCHAR(50) NOT NULL DEFAULT 'creating',
    claimed_by UUID,
    claimed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    last_used_at TIMESTAMPTZ,
    error_message TEXT,
    version INTEGER DEFAULT 1
);

-- Execution requests
CREATE TABLE IF NOT EXISTS executions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id VARCHAR(255) NOT NULL,
    chat_id VARCHAR(255) NOT NULL,
    sandbox_id UUID REFERENCES sandbox_pool(id),
    language VARCHAR(50) NOT NULL,
    path VARCHAR(255) NOT NULL DEFAULT 'main.py',
    code TEXT NOT NULL,
    status VARCHAR(50) NOT NULL DEFAULT 'pending',
    stdout TEXT,
    stderr TEXT,
    exit_code INTEGER,
    execution_time_ms INTEGER,
    input_files JSONB,
    output_files JSONB,
    timeout_seconds INTEGER DEFAULT 300,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ
);

-- Indexes
CREATE INDEX IF NOT EXISTS idx_sandbox_state ON sandbox_pool(state);
CREATE INDEX IF NOT EXISTS idx_sandbox_available ON sandbox_pool(state, created_at) WHERE state = 'available';
CREATE INDEX IF NOT EXISTS idx_executions_status ON executions(status);
CREATE INDEX IF NOT EXISTS idx_executions_user_chat ON executions(user_id, chat_id);
