package agentcfg

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// LoadFromDir reads all .yaml/.yml files from dir, applies defaults,
// validates each, and returns the successfully loaded configs sorted by name.
// If some files fail, the valid configs are still returned alongside an error
// summarizing the failures. Configs whose top-level name is declared by more
// than one successfully loaded file are all excluded and reported in the
// error; a file that fails to load takes no part in duplicate detection (its
// config never enters the returned slice, so it cannot be resolved by name).
func LoadFromDir(dir string) ([]AgentConfig, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read agentcfg dir %s: %w", dir, err)
	}

	var configs []AgentConfig
	var fileErrs []string
	filesByName := make(map[string][]string)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		path := filepath.Join(dir, name)
		cfg, ferr := LoadFile(path)
		if ferr != nil {
			fileErrs = append(fileErrs, fmt.Sprintf("%s: %v", name, ferr))
			continue
		}
		configs = append(configs, cfg)
		filesByName[cfg.Name] = append(filesByName[cfg.Name], name)
	}

	// Reject duplicate top-level names fail-closed: every by-name lookup over
	// the returned slice (MCP secret hydration, Schedule registration, the
	// configs API) is first-match-wins, so two loaded files sharing a name
	// could resolve to the wrong file's config and inject its MCP secrets
	// into a different task's runner. Neither file can be presumed the
	// intended one, so all configs bearing a duplicated name are excluded;
	// uniquely named configs still load, matching the per-file partial-load
	// semantics above. Only successfully loaded configs are considered: a
	// file that failed LoadFile is already absent from the slice, so it
	// cannot make a name ambiguous.
	dupNames := make(map[string]bool)
	for cfgName, files := range filesByName {
		if len(files) > 1 {
			dupNames[cfgName] = true
		}
	}
	if len(dupNames) > 0 {
		configs = slices.DeleteFunc(configs, func(c AgentConfig) bool { return dupNames[c.Name] })
		for _, cfgName := range slices.Sorted(maps.Keys(dupNames)) {
			// entries is sorted by file name (os.ReadDir), so the list reads
			// deterministically.
			fileErrs = append(fileErrs, fmt.Sprintf(
				"duplicate config name %q declared by %s (all of them excluded)",
				cfgName, strings.Join(filesByName[cfgName], ", ")))
		}
	}

	slices.SortFunc(configs, func(a, b AgentConfig) int {
		return strings.Compare(a.Name, b.Name)
	})

	if len(fileErrs) > 0 {
		return configs, fmt.Errorf("agentcfg load errors: %s", strings.Join(fileErrs, "; "))
	}
	return configs, nil
}

// LoadFile reads a single YAML file, unmarshals it into an AgentConfig,
// applies defaults, and validates the result.
func LoadFile(path string) (AgentConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return AgentConfig{}, err
	}
	var cfg AgentConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return AgentConfig{}, fmt.Errorf("yaml parse: %w", err)
	}
	if err := ApplyDefaults(&cfg); err != nil {
		return AgentConfig{}, err
	}
	if err := Validate(&cfg); err != nil {
		return AgentConfig{}, err
	}
	return cfg, nil
}

// ApplyDefaults fills in zero-valued fields with sensible defaults.
func ApplyDefaults(cfg *AgentConfig) error {
	if cfg.Concurrency.MaxConcurrent == 0 {
		cfg.Concurrency.MaxConcurrent = 1
	}
	if cfg.Concurrency.OnConflict == "" {
		cfg.Concurrency.OnConflict = "skip"
	}
	applyPhaseDefaults(cfg)
	applyMonitorDefaults(cfg)
	// A supervisor's cli sub-agents run phases too, so give their phases the
	// same steering.mode / output.capture / fanout / runner-flag defaults a
	// top-level phase gets. Without this, a sub-agent phase that omits those
	// fields is rejected by Validate with a confusing `invalid steering.mode
	// ""` instead of defaulting. Concurrency and Monitor are top-level-only
	// concepts and are not recursed; native sub-agents have no phases, so the
	// call is a no-op for them.
	for kind, sub := range cfg.Supervisor.SubAgents { //nolint:gocritic // rangeValCopy: SubAgents is a handful of kinds; we default the copy and write it back into the map
		applyPhaseDefaults(&sub)
		cfg.Supervisor.SubAgents[kind] = sub
	}
	return nil
}

// applyPhaseDefaults fills in per-phase enum defaults (steering.mode,
// output.capture), fanout parallelism, and runner-flag defaults for every
// phase in cfg. Shared by ApplyDefaults for both the top-level task and each
// supervisor sub-agent so their phases default identically.
func applyPhaseDefaults(cfg *AgentConfig) {
	for i := range cfg.Phases {
		p := &cfg.Phases[i]
		if p.Steering.Mode == "" {
			p.Steering.Mode = SteeringScheduled
		}
		if p.Output.Capture == "" {
			p.Output.Capture = OutputCaptureText
		}
		if p.Fanout != nil && p.Fanout.MaxParallel == 0 {
			p.Fanout.MaxParallel = 1
		}
	}
	applyRunnerFlagDefaults(cfg)
}

// applyMonitorDefaults fills in the queue_monitor dispatch limits when unset
// (or set to a non-positive value). Applied unconditionally, mirroring how
// Concurrency defaults are applied regardless of strategy: the block is
// simply unused by external_agent tasks.
func applyMonitorDefaults(cfg *AgentConfig) {
	if cfg.Monitor.MaxParallelCases <= 0 {
		cfg.Monitor.MaxParallelCases = 5
	}
	if cfg.Monitor.MaxNewCasesPerPass <= 0 {
		cfg.Monitor.MaxNewCasesPerPass = 20
	}
	if cfg.Monitor.MaxCaseAttempts <= 0 {
		cfg.Monitor.MaxCaseAttempts = 3
	}
}

func applyRunnerFlagDefaults(cfg *AgentConfig) {
	for i := range cfg.Phases {
		applyRunnerFlagDefaultsToFlags(&cfg.Phases[i].RunnerFlags)
		if cfg.Phases[i].Fanout != nil {
			applyRunnerFlagDefaultsToFlags(&cfg.Phases[i].Fanout.Child.RunnerFlags)
		}
	}
}

func applyRunnerFlagDefaultsToFlags(rf *RunnerFlags) {
	if rf.Bare == nil {
		rf.Bare = boolPtr(true)
	}
	if rf.StrictMCPConfig == nil {
		rf.StrictMCPConfig = boolPtr(true)
	}
	if rf.NoSessionPersistence == nil {
		rf.NoSessionPersistence = boolPtr(true)
	}
}

func boolPtr(v bool) *bool { return &v }

// knownRunners is the canonical set of runner names accepted by Validate.
// It mirrors the runner identifiers exposed by internal/agent/runner/* (the
// values each Runner.Name returns). Stored as a fixed-size array so external
// packages cannot mutate the set; access through KnownRunners and
// IsKnownRunner.
var knownRunners = [...]string{"claude", "copilot", "gemini"}

// KnownRunners returns a freshly allocated slice of the runner names
// accepted by Validate. Callers receive a copy, so mutating the slice has no
// effect on validation behavior.
func KnownRunners() []string {
	out := make([]string, len(knownRunners))
	copy(out, knownRunners[:])
	return out
}

// IsKnownRunner reports whether name is one of the runner identifiers
// recognized by Validate.
func IsKnownRunner(name string) bool {
	return slices.Contains(knownRunners[:], name)
}

// Validate checks that a config has all required fields and that enum values
// are within the allowed set. The required fields depend on Strategy:
// external_agent runs phases via a CLI runner; queue_monitor dispatches Case
// children from a prefilter and never runs phases itself.
func Validate(cfg *AgentConfig) error {
	if strings.TrimSpace(cfg.Name) == "" {
		return errors.New("name is required")
	}
	if cfg.Backend != "" {
		return fmt.Errorf("backend is only valid on supervisor sub-agents, not top-level tasks (got %q)", cfg.Backend)
	}
	var strategyErr error
	switch cfg.Strategy {
	case StrategyExternalAgent:
		strategyErr = validateExternalAgent(cfg)
	case StrategyQueueMonitor:
		strategyErr = validateQueueMonitor(cfg)
	default:
		return fmt.Errorf("strategy must be %q or %q, got %q", StrategyExternalAgent, StrategyQueueMonitor, cfg.Strategy)
	}
	if strategyErr != nil {
		return strategyErr
	}
	if supervisorIsSet(&cfg.Supervisor) {
		return validateSupervisor(&cfg.Supervisor)
	}
	return nil
}

// supervisorIsSet reports whether cfg.Supervisor was configured at all, so
// Validate only runs supervisor-specific checks for tasks that actually use
// the Case Supervisor.
func supervisorIsSet(s *Supervisor) bool {
	return s.Model != "" || s.Prompt.Base != "" || s.MaxRounds != 0 ||
		s.CostCapUSD != 0 || s.MaxDuration != 0 || len(s.Tools) != 0 || len(s.SubAgents) != 0
}

// validateSupervisor checks the Case Supervisor block: each declared command
// tool's name and argv template, and each sub-agent's config. MaxRounds,
// CostCapUSD, and MaxDuration may be zero; the workflow applies defaults.
func validateSupervisor(s *Supervisor) error {
	if err := validateDeclaredToolList("supervisor.tools", s.Tools, ReservedToolSpawnAgent, ReservedToolRecordOutcome); err != nil {
		return err
	}

	for name, sub := range s.SubAgents { //nolint:gocritic // rangeValCopy: SubAgents is a handful of kinds; the validators need a pointer, and copying keeps the loop body simple
		subCopy := sub
		var err error
		switch BackendOf(&subCopy) {
		case BackendCLI:
			err = validateExternalAgent(&subCopy)
		case BackendNative:
			err = validateNativeAgent(&subCopy)
		default:
			err = fmt.Errorf("backend must be %q or %q, got %q", BackendCLI, BackendNative, subCopy.Backend)
		}
		if err != nil {
			return fmt.Errorf("supervisor.sub_agents[%s]: %w", name, err)
		}
	}
	return nil
}

// validateDeclaredToolList checks a declared-tool list: names present, no
// collision with the loop's reserved built-in tool names, no duplicates, and
// each argv template valid (validateDeclaredTool). listPrefix is the config
// path used in error messages ("supervisor.tools" or "native.tools").
func validateDeclaredToolList(listPrefix string, tools []DeclaredTool, reserved ...string) error {
	seen := make(map[string]int, len(tools))
	for i := range tools {
		t := &tools[i]
		if strings.TrimSpace(t.Name) == "" {
			return fmt.Errorf("%s[%d]: name is required", listPrefix, i)
		}
		if slices.Contains(reserved, t.Name) {
			return fmt.Errorf("%s[%d]: name %q collides with a reserved built-in tool", listPrefix, i, t.Name)
		}
		if prev, dup := seen[t.Name]; dup {
			return fmt.Errorf("%s[%d] (%s): duplicate name (first at %s[%d])", listPrefix, i, t.Name, listPrefix, prev)
		}
		seen[t.Name] = i
		if err := validateDeclaredTool(fmt.Sprintf("%s[%d] (%s)", listPrefix, i, t.Name), t); err != nil {
			return err
		}
	}
	return nil
}

// validateDeclaredTool checks a single DeclaredTool's argv template. prefix is
// the error-message context supplied by the caller ("supervisor.tools[0] (t)"
// or "native.tools[0] (t)") so the same checks serve both tool lists.
func validateDeclaredTool(prefix string, t *DeclaredTool) error {
	if len(t.Command) == 0 {
		return fmt.Errorf("%s: command is required", prefix)
	}
	// A whole-element placeholder as the executable would hand the choice of
	// binary to the LLM: the pre-"--" dash guard at exec time only rejects
	// values starting with "-", not an arbitrary substituted path.
	if IsWholeElementPlaceholder(t.Command[0]) {
		return fmt.Errorf("%s: command[0] (the executable) must be a literal, not a placeholder", prefix)
	}
	hasTerminator := false
	for _, elem := range t.Command {
		if elem == "--" {
			hasTerminator = true
			continue
		}
		if IsWholeElementPlaceholder(elem) {
			continue
		}
		if strings.ContainsAny(elem, "{}") {
			return fmt.Errorf("%s: command element %q has embedded placeholder syntax; a placeholder must be a whole element like \"{name}\"", prefix, elem)
		}
	}
	if !hasTerminator {
		return fmt.Errorf("%s: command must contain a \"--\" flag terminator element", prefix)
	}
	return validateDeclaredToolPlaceholders(prefix, t)
}

// validateDeclaredToolPlaceholders cross-checks every {placeholder} in the
// argv template against the declared args_schema: RunDeclaredCommand
// substitutes each whole-element placeholder from the tool-call arguments
// (bound to args_schema) at exec time, so a placeholder with no matching
// property can never be satisfied. Catching it here turns a per-call
// rejectedResult (e.g. a typo like {qeury} vs a "query" property) into an
// upfront config error.
func validateDeclaredToolPlaceholders(prefix string, t *DeclaredTool) error {
	// A "required" key that is not a list would silently read as "nothing is
	// required" both here and in the LLM-facing schema builder, turning every
	// placeholder check below into a misleading "not listed" error. Reject
	// the malformed shape with a direct message instead.
	if rawReq, present := t.ArgsSchema["required"]; present {
		if _, isList := rawReq.([]any); !isList {
			return fmt.Errorf("%s: args_schema.required must be a list of property names (got %T)", prefix, rawReq)
		}
	}
	props := SchemaProperties(t.ArgsSchema["properties"])
	required := SchemaStringList(t.ArgsSchema["required"])
	for _, elem := range t.Command {
		if !IsWholeElementPlaceholder(elem) {
			continue
		}
		name := elem[1 : len(elem)-1]
		raw, present := props[name]
		if !present {
			return fmt.Errorf("%s: command placeholder %q has no matching property in args_schema", prefix, elem)
		}
		// The property must be a JSON-Schema object, not just any value: the
		// LLM-facing schema builder (argsSchemaToLLMSchema in internal/workflow)
		// drops a property whose value is not an object, so a non-object
		// property is invisible to the LLM and its placeholder is just as
		// unsatisfiable at exec time as a missing one.
		if _, isObject := raw.(map[string]any); !isObject {
			return fmt.Errorf("%s: command placeholder %q maps to args_schema property %q, which is not a schema object", prefix, elem, name)
		}
		// The property must also be required: substitution hard-rejects a call
		// whose arguments omit a placeholder's value, so a placeholder bound to
		// an optional property only works when the LLM happens to supply the
		// value and is a per-call rejection whenever it legitimately omits it.
		// Requiring the property trades that fragile shape for a load error.
		if !slices.Contains(required, name) {
			return fmt.Errorf("%s: command placeholder %q maps to args_schema property %q, which is not listed in args_schema.required", prefix, elem, name)
		}
	}
	return nil
}

// validateNativeAgent checks a backend: native sub-agent config: a prompt base
// (required: unlike the optional supervisor block, a native sub-agent is
// explicitly declared, so shipping one without a prompt is a config defect;
// the workflow's built-in default instruction stays as defense in depth only),
// a declared output schema (submit_result binds its parameters to it), and
// each declared command tool's argv template. Model, MaxRounds, CostCapUSD,
// and MaxDuration may be zero; the workflow applies defaults.
func validateNativeAgent(cfg *AgentConfig) error {
	na := &cfg.Native
	if na.Prompt.Base == "" {
		return errors.New("native.prompt.base is required")
	}
	if na.Output.Schema == nil {
		return errors.New("native.output.schema is required (submit_result binds to it)")
	}
	return validateDeclaredToolList("native.tools", na.Tools, ReservedToolSubmitResult, ReservedToolReportFailure)
}

func validateExternalAgent(cfg *AgentConfig) error {
	if cfg.Runner == "" {
		return errors.New("runner is required")
	}
	if !IsKnownRunner(cfg.Runner) {
		return fmt.Errorf("runner must be one of %v, got %q", KnownRunners(), cfg.Runner)
	}
	if len(cfg.Phases) == 0 {
		return errors.New("at least one phase is required")
	}
	seenPhase := make(map[string]int, len(cfg.Phases))
	for i := range cfg.Phases {
		p := &cfg.Phases[i]
		if prev, dup := seenPhase[p.Name]; strings.TrimSpace(p.Name) != "" && dup {
			return fmt.Errorf("phases[%d] (%s): duplicate name (first at phases[%d])", i, p.Name, prev)
		}
		if err := validatePhase(i, p, seenPhase); err != nil {
			return err
		}
		// Registered only after validation so a phase's fanout.over cannot
		// satisfy the "preceding phase" rule by referencing its own name.
		seenPhase[p.Name] = i
	}
	return nil
}

// validatePhase checks the required fields and enum values of a single
// external_agent phase. seenPhase is the running duplicate-name and
// fanout.over reference table built by validateExternalAgent.
func validatePhase(i int, p *Phase, seenPhase map[string]int) error {
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("phases[%d]: name is required", i)
	}
	if p.Model == "" {
		return fmt.Errorf("phases[%d] (%s): model is required", i, p.Name)
	}
	if p.Prompt.Base == "" {
		return fmt.Errorf("phases[%d] (%s): prompt.base is required", i, p.Name)
	}
	switch p.Steering.Mode {
	case SteeringScheduled, SteeringInteractive:
	default:
		return fmt.Errorf("phases[%d] (%s): invalid steering.mode %q", i, p.Name, p.Steering.Mode)
	}
	switch p.Output.Capture {
	case OutputCaptureText, OutputCaptureStructured:
	default:
		return fmt.Errorf("phases[%d] (%s): invalid output.capture %q", i, p.Name, p.Output.Capture)
	}
	if p.Fanout != nil {
		if err := validateFanout(i, p.Name, p.Fanout, seenPhase); err != nil {
			return err
		}
	}
	for name, srv := range p.Tools.MCP.Servers {
		if err := validateMCPServer(i, p.Name, name, srv); err != nil {
			return err
		}
	}
	return nil
}

// validateQueueMonitor checks the fields a queue_monitor task needs: a
// prefilter command to emit candidates and a cron schedule to fire the
// dispatcher pass. It does not require a runner or phases, since the
// monitor never runs an agent CLI itself.
func validateQueueMonitor(cfg *AgentConfig) error {
	if cfg.Prefilter.Command == "" {
		return errors.New("prefilter.command is required for strategy 'queue_monitor'")
	}
	if cfg.Schedule.Cron == "" {
		return errors.New("schedule.cron is required for strategy 'queue_monitor'")
	}
	return nil
}

func validateFanout(phaseIdx int, phaseName string, fc *FanoutConfig, seenPhases map[string]int) error {
	prefix := fmt.Sprintf("phases[%d] (%s)", phaseIdx, phaseName)
	if fc.Over == "" {
		return fmt.Errorf("%s: fanout.over is required", prefix)
	}
	rootKey := fc.Over
	if dot := strings.IndexByte(fc.Over, '.'); dot > 0 {
		rootKey = fc.Over[:dot]
	}
	if rootKey != "prefilter" {
		if _, ok := seenPhases[rootKey]; !ok {
			return fmt.Errorf("%s: fanout.over references %q which is not a preceding phase or \"prefilter\"", prefix, rootKey)
		}
	}
	if fc.Child.Model == "" {
		return fmt.Errorf("%s: fanout.child.model is required", prefix)
	}
	if fc.Child.Prompt.Base == "" {
		return fmt.Errorf("%s: fanout.child.prompt.base is required", prefix)
	}
	switch fc.Child.Steering.Mode {
	case SteeringScheduled, SteeringInteractive, "":
	default:
		return fmt.Errorf("%s: fanout.child: invalid steering.mode %q", prefix, fc.Child.Steering.Mode)
	}
	for name, srv := range fc.Child.Tools.MCP.Servers {
		if err := validateMCPServer(phaseIdx, phaseName+".fanout.child", name, srv); err != nil {
			return err
		}
	}
	return nil
}

func validateMCPServer(phaseIdx int, phaseName, serverName string, srv MCPServerConfig) error {
	prefix := fmt.Sprintf("phases[%d] (%s): mcp.servers[%s]", phaseIdx, phaseName, serverName)
	switch srv.Type {
	case "http":
		if srv.URL == "" {
			return fmt.Errorf("%s: url is required for type \"http\"", prefix)
		}
	case "stdio":
		if srv.Command == "" {
			return fmt.Errorf("%s: command is required for type \"stdio\"", prefix)
		}
	case "":
		if srv.URL != "" && srv.Command != "" {
			return fmt.Errorf("%s: set type to \"http\" or \"stdio\" when both url and command are present", prefix)
		}
		if srv.URL == "" && srv.Command == "" {
			return fmt.Errorf("%s: must set either url (http) or command (stdio)", prefix)
		}
	default:
		return fmt.Errorf("%s: unknown type %q (expected \"http\" or \"stdio\")", prefix, srv.Type)
	}
	return nil
}
