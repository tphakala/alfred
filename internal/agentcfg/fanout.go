package agentcfg

// FanoutConfig defines a phase that dispatches child workflows instead of
// running ExecAgentCLI directly. Each item from the array at the Over path
// becomes one child workflow.
type FanoutConfig struct {
	Over        string            `yaml:"over"`         // dot-path into PhaseOutputs (e.g. "scout.subtasks")
	MaxParallel int               `yaml:"max_parallel"` // max concurrent child workflows
	Child       FanoutChildConfig `yaml:"child"`
}

// FanoutChildConfig is the per-child subset of Phase. The parent Phase's
// top-level fields (Name, Model, Steering) are ignored when Fanout is set;
// the child config is used instead.
type FanoutChildConfig struct {
	Runner      string      `yaml:"runner,omitempty"` // defaults to parent AgentConfig.Runner
	Model       string      `yaml:"model"`
	Prompt      Prompt      `yaml:"prompt"`
	Tools       Tools       `yaml:"tools,omitempty"`
	RunnerFlags RunnerFlags `yaml:"runner_flags,omitempty"`
	Steering    Steering    `yaml:"steering,omitempty"`
	Output      Output      `yaml:"output,omitempty"`
}
