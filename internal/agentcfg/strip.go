package agentcfg

// StripMCPSecrets returns a copy of ac with Headers and Env nil'd on all
// MCP server configs across all phases. The original config is not mutated.
//
// Use this before passing AgentConfig to Temporal (workflow input, activity
// args) to prevent secrets from appearing in workflow history.
func StripMCPSecrets(ac AgentConfig) AgentConfig { //nolint:gocritic // value param is intentional: caller's copy is the starting point
	stripped := ac
	stripped.Phases = make([]Phase, len(ac.Phases))
	copy(stripped.Phases, ac.Phases)
	for i := range stripped.Phases {
		rf := &stripped.Phases[i].RunnerFlags
		rf.Bare = cloneBoolPtr(rf.Bare)
		rf.StrictMCPConfig = cloneBoolPtr(rf.StrictMCPConfig)
		rf.NoSessionPersistence = cloneBoolPtr(rf.NoSessionPersistence)

		if len(stripped.Phases[i].Tools.MCP.Servers) == 0 {
			continue
		}
		servers := make(map[string]MCPServerConfig, len(stripped.Phases[i].Tools.MCP.Servers))
		for name, cfg := range stripped.Phases[i].Tools.MCP.Servers {
			cfg.Headers = nil
			cfg.Env = nil
			servers[name] = cfg
		}
		stripped.Phases[i].Tools.MCP.Servers = servers
	}

	if len(ac.Supervisor.SubAgents) > 0 {
		strippedSubAgents := make(map[string]AgentConfig, len(ac.Supervisor.SubAgents))
		for name, sub := range ac.Supervisor.SubAgents { //nolint:gocritic // rangeValCopy: SubAgents is a handful of kinds at most; StripMCPSecrets takes AgentConfig by value everywhere else in this file
			// Recurse: a sub-agent is itself an AgentConfig, so the same
			// per-phase MCP stripping applies to it. Supervisor.Tools (the
			// declared command tools) is left as-is: DeclaredTool.Env holds
			// ${secret:...} references, not raw secrets, so it is already
			// safe to carry into Temporal history.
			strippedSubAgents[name] = StripMCPSecrets(sub)
		}
		stripped.Supervisor.SubAgents = strippedSubAgents
	}

	return stripped
}

func cloneBoolPtr(p *bool) *bool {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
