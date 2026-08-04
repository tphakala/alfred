CREATE TABLE task_state (
    task           TEXT NOT NULL,
    candidate_key  TEXT NOT NULL,
    status         TEXT NOT NULL CHECK (status IN ('in_progress', 'done', 'skipped', 'escalated', 'needs_attention')),
    outcome        JSONB,
    case_run_id    TEXT,
    attempts       INT NOT NULL DEFAULT 0,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (task, candidate_key)
);

CREATE INDEX idx_task_state_task_status ON task_state (task, status);
