-- This rollback fails if any sessions.kind = 'case' rows exist: the
-- reinstated CHECK constraint below does not allow 'case', so operators must
-- ensure there are no live case sessions before rolling back this migration.
ALTER TABLE sessions DROP CONSTRAINT sessions_kind_check;
ALTER TABLE sessions ADD CONSTRAINT sessions_kind_check
	CHECK (kind IN ('chat', 'ticket', 'monitor', 'agent_task'));
