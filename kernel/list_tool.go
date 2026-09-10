package kernel

import (
	"context"
	"encoding/json"
	"fmt"

	openagent "github.com/yusheng-g/openagent-go"
)

// listTool implements the "sub_agent_list" tool: it lists all live sub-agents
// in this session, showing which are currently running and which are idle
// (resumable via sub_agent_send). Sub-agents that were killed (session close)
// are absent — their absence signals they're gone.
type listTool struct {
	reg *childRegistry
}

// listParams has no parameters — sub_agent_list takes no arguments.
type listParams struct{}

func newListTool(reg *childRegistry) *listTool {
	return &listTool{reg: reg}
}

func (t *listTool) Definition() openagent.FunctionDefinition {
	return openagent.FunctionDefinition{
		Name: "sub_agent_list",
		Description: "List all live sub-agents in this session and their status. " +
			"Running sub-agents are still executing; idle ones finished and are resumable via sub_agent_send. " +
			"A sub-agent not in the list has been killed or expired — launch a fresh one instead of sending to it.",
		Parameters: openagent.SchemaOf[listParams](),
	}
}

func (t *listTool) Execute(_ context.Context, _ json.RawMessage) *openagent.ToolResult {
	agents := t.reg.List()
	if len(agents) == 0 {
		return &openagent.ToolResult{Content: "No live sub-agents in this session."}
	}
	var b []byte
	b, _ = json.MarshalIndent(agents, "", "  ")
	running := 0
	for _, a := range agents {
		if a.Running {
			running++
		}
	}
	hint := ""
	if running > 0 {
		hint = "\n\nNote: " + fmt.Sprintf("%d sub-agent(s) are still running. You will receive a system-reminder when each completes — do not poll. If you are blocked waiting for a result and have no other work to do, stop and let the user know you're waiting; the reminder will arrive automatically.", running)
	}
	return &openagent.ToolResult{Content: fmt.Sprintf("Live sub-agents (%d):\n%s%s", len(agents), string(b), hint)}
}
