DROP TRIGGER IF EXISTS trg_arm_task_stop_lease ON agent_task_queue;
DROP FUNCTION IF EXISTS arm_task_stop_lease_on_cancel();
ALTER TABLE agent_task_queue DROP COLUMN IF EXISTS stop_lease_expires_at;
