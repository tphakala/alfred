ALTER TABLE sessions DROP CONSTRAINT sessions_kind_check;
ALTER TABLE sessions ADD CONSTRAINT sessions_kind_check
	CHECK (kind IN ('chat', 'ticket', 'monitor', 'agent_task', 'case'));
