package agent

import (
	"errors"
	"fmt"
	"strings"
)

// Skill is model-facing guidance loaded into the system prompt at startup.
type Skill struct {
	Name         string
	Instructions string
}

func (a *Agent) loadSkills(skills []Skill) error {
	names := make(map[string]struct{}, len(skills))
	for _, skill := range skills {
		skill.Name = strings.TrimSpace(skill.Name)
		skill.Instructions = strings.TrimSpace(skill.Instructions)
		if skill.Name == "" || skill.Instructions == "" {
			return errors.New("agent: skill name and instructions are required")
		}
		if _, exists := names[skill.Name]; exists {
			return fmt.Errorf("agent: duplicate skill %q", skill.Name)
		}
		names[skill.Name] = struct{}{}
		if a.systemPrompt != "" {
			a.systemPrompt += "\n\n"
		}
		a.systemPrompt += skill.Instructions
	}
	return nil
}
