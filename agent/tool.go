package agent

import (
	"context"
	"encoding/json"
	"fmt"
)

// Tool is everything the runtime needs to describe and execute a tool.
type Tool struct {
	Name        string
	Description string
	InputSchema string
	Execute     func(context.Context, string) (string, error)
}

func (a *Agent) registerTools(tools []Tool) error {
	for _, tool := range tools {
		if tool.Name == "" || tool.Description == "" || !json.Valid([]byte(tool.InputSchema)) || tool.Execute == nil {
			return fmt.Errorf("agent: invalid tool %q", tool.Name)
		}
		if _, exists := a.tools[tool.Name]; exists {
			return fmt.Errorf("agent: duplicate tool %q", tool.Name)
		}
		a.tools[tool.Name] = tool
		tool.Execute = nil
		a.definitions = append(a.definitions, tool)
	}
	return nil
}
