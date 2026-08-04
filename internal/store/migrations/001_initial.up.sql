CREATE TABLE sessions (
    id             UUID PRIMARY KEY,
    workflow_id    TEXT NOT NULL,
    run_id         TEXT,
    parent_session UUID REFERENCES sessions(id),
    kind           TEXT NOT NULL CHECK (kind IN ('chat', 'ticket', 'monitor', 'agent_task')),
    status         TEXT NOT NULL CHECK (status IN ('active', 'completed', 'continued')),
    epoch          INT NOT NULL DEFAULT 1,
    epoch_summary  TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_sessions_workflow_id ON sessions(workflow_id);
CREATE INDEX idx_sessions_status ON sessions(status);

CREATE TABLE messages (
    id              UUID PRIMARY KEY,
    session_id      UUID NOT NULL REFERENCES sessions(id),
    sequence        INT NOT NULL,
    role            TEXT NOT NULL CHECK (role IN ('user', 'model', 'tool_call', 'tool_result', 'context', 'approval_request', 'approval_result', 'error')),
    content         TEXT NOT NULL,
    token_estimate  INT,
    metadata        JSONB,
    idempotency_key TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(session_id, sequence)
);

CREATE UNIQUE INDEX idx_messages_idempotency
    ON messages (session_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

CREATE TABLE epoch_transitions (
    id              UUID PRIMARY KEY,
    from_session    UUID NOT NULL,
    to_session      UUID NOT NULL,
    trigger         TEXT NOT NULL,
    extracted_facts TEXT,
    summary         TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_epoch_transitions_from ON epoch_transitions(from_session);
