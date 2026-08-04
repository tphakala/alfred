package agentcfg

import "fmt"

// ScheduleID is the Temporal Schedule ID for an AgentConfig.
func ScheduleID(configName string) string {
	return fmt.Sprintf("agent_task:%s", configName)
}

// WorkflowID is the Temporal Workflow ID for a single run of an AgentConfig.
func WorkflowID(configName, triggerISO string) string {
	return fmt.Sprintf("agent_task-%s-%s", configName, triggerISO)
}

// CaseWorkflowID is the deterministic Temporal Workflow ID for a Case
// workflow dispatched by a queue_monitor task, derived from (task,
// candidateKey). Temporal itself rejects a duplicate live case for the same
// (task, candidateKey) using this ID, which is the dedup guarantee.
func CaseWorkflowID(task, candidateKey string) string {
	return fmt.Sprintf("case-%s-%s", task, candidateKey)
}
