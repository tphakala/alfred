package agentcfg

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStripMCPSecrets_RemovesHeadersAndEnv(t *testing.T) {
	original := AgentConfig{
		Name: "test",
		Phases: []Phase{{
			Name: "main",
			Tools: Tools{
				MCP: MCP{Servers: map[string]MCPServerConfig{
					"hindsight": {
						Type:    "http",
						URL:     "http://localhost:8890/mcp/hindsight/",
						Headers: map[string]string{"Authorization": "Bearer secret-token"},
						Env:     map[string]string{"API_KEY": "sk-secret"},
					},
				}},
			},
		}},
	}

	stripped := StripMCPSecrets(original)

	srv := stripped.Phases[0].Tools.MCP.Servers["hindsight"]
	assert.Equal(t, "http", srv.Type)
	assert.Equal(t, "http://localhost:8890/mcp/hindsight/", srv.URL)
	assert.Nil(t, srv.Headers, "Headers should be nil after stripping")
	assert.Nil(t, srv.Env, "Env should be nil after stripping")

	origSrv := original.Phases[0].Tools.MCP.Servers["hindsight"]
	require.NotNil(t, origSrv.Headers, "original Headers must not be mutated")
	assert.Equal(t, "Bearer secret-token", origSrv.Headers["Authorization"])
	require.NotNil(t, origSrv.Env, "original Env must not be mutated")
	assert.Equal(t, "sk-secret", origSrv.Env["API_KEY"])
}

func TestStripMCPSecrets_BoolPtrFieldsNotAliased(t *testing.T) {
	tr := true
	original := AgentConfig{
		Name: "alias-test",
		Phases: []Phase{{
			Name:        "main",
			RunnerFlags: RunnerFlags{Bare: &tr, StrictMCPConfig: &tr, NoSessionPersistence: &tr},
		}},
	}

	stripped := StripMCPSecrets(original)

	f := false
	*stripped.Phases[0].RunnerFlags.Bare = f

	assert.True(t, *original.Phases[0].RunnerFlags.Bare,
		"mutating stripped copy must not affect the original")
}

func TestStripMCPSecrets_NoMCPServersNoOp(t *testing.T) {
	original := AgentConfig{
		Name: "no-mcp",
		Phases: []Phase{{
			Name:  "main",
			Tools: Tools{Bash: []string{"git"}},
		}},
	}

	stripped := StripMCPSecrets(original)

	assert.Equal(t, "no-mcp", stripped.Name)
	assert.Len(t, stripped.Phases, 1)
	assert.Equal(t, []string{"git"}, stripped.Phases[0].Tools.Bash)
	assert.Empty(t, stripped.Phases[0].Tools.MCP.Servers)
}

func TestStripMCPSecrets_MultiplePhases(t *testing.T) {
	original := AgentConfig{
		Name: "multi",
		Phases: []Phase{
			{
				Name: "phase-with-secrets",
				Tools: Tools{
					MCP: MCP{Servers: map[string]MCPServerConfig{
						"a": {
							URL:     "http://a",
							Headers: map[string]string{"X-Token": "secret-a"},
						},
					}},
				},
			},
			{
				Name:  "phase-without-mcp",
				Tools: Tools{Bash: []string{"echo"}},
			},
			{
				Name: "phase-with-env-only",
				Tools: Tools{
					MCP: MCP{Servers: map[string]MCPServerConfig{
						"b": {
							Command: "npx",
							Args:    []string{"-y", "server"},
							Env:     map[string]string{"SECRET": "val"},
						},
					}},
				},
			},
		},
	}

	stripped := StripMCPSecrets(original)

	require.Len(t, stripped.Phases, 3)

	srvA := stripped.Phases[0].Tools.MCP.Servers["a"]
	assert.Equal(t, "http://a", srvA.URL)
	assert.Nil(t, srvA.Headers)

	assert.Equal(t, []string{"echo"}, stripped.Phases[1].Tools.Bash)

	srvB := stripped.Phases[2].Tools.MCP.Servers["b"]
	assert.Equal(t, "npx", srvB.Command)
	assert.Equal(t, []string{"-y", "server"}, srvB.Args)
	assert.Nil(t, srvB.Env)

	origA := original.Phases[0].Tools.MCP.Servers["a"]
	assert.Equal(t, "secret-a", origA.Headers["X-Token"], "original must not be mutated")
	origB := original.Phases[2].Tools.MCP.Servers["b"]
	assert.Equal(t, "val", origB.Env["SECRET"], "original must not be mutated")
}

func TestStripMCPSecrets_SubAgentMCPStripped(t *testing.T) {
	original := AgentConfig{
		Name: "with-supervisor",
		Supervisor: Supervisor{
			LoopConfig: LoopConfig{
				Model: testAgentModel,
				Tools: []DeclaredTool{{
					Name:    testToolCmdLookup,
					Command: []string{testToolCmdLookup, "--", testPlaceholderQuery},
					Env:     map[string]string{"TOKEN": "${secret:lookup-token}"},
				}},
			},
			SubAgents: map[string]AgentConfig{
				testSubAgentKind: {
					Name:   testSubAgentKind,
					Runner: testAgentRunner,
					Phases: []Phase{{
						Name: testAgentPhaseMain,
						Tools: Tools{
							MCP: MCP{Servers: map[string]MCPServerConfig{
								"hindsight": {
									Type:    "http",
									URL:     "http://localhost:8890/mcp/hindsight/",
									Headers: map[string]string{"Authorization": "Bearer secret-token"},
									Env:     map[string]string{"API_KEY": "sk-secret"},
								},
							}},
						},
					}},
				},
			},
		},
	}

	stripped := StripMCPSecrets(original)

	sub := stripped.Supervisor.SubAgents[testSubAgentKind]
	srv := sub.Phases[0].Tools.MCP.Servers["hindsight"]
	assert.Equal(t, "http", srv.Type)
	assert.Nil(t, srv.Headers, "sub-agent MCP Headers should be nil after stripping")
	assert.Nil(t, srv.Env, "sub-agent MCP Env should be nil after stripping")

	origSrv := original.Supervisor.SubAgents[testSubAgentKind].Phases[0].Tools.MCP.Servers["hindsight"]
	require.NotNil(t, origSrv.Env, "original sub-agent Env must not be mutated")
	assert.Equal(t, "sk-secret", origSrv.Env["API_KEY"])

	// DeclaredTool.Env holds a ${secret:...} reference, not a raw secret, so
	// it must survive stripping unchanged.
	require.Len(t, stripped.Supervisor.Tools, 1)
	assert.Equal(t, "${secret:lookup-token}", stripped.Supervisor.Tools[0].Env["TOKEN"])
}
