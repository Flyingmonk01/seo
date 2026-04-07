// Package agent defines the multi-agent architecture interfaces and base types.
// This is an additive layer — it wraps existing service logic without replacing it.
// Execution order: Orchestrator → Planner → Workers (SEO/Code) → Critic
package agent

import "context"

// Agent is the contract every agent must satisfy.
// Inputs and outputs are typed via AgentInput.Payload / AgentOutput.Data
// using concrete structs defined in types.go.
type Agent interface {
	// Name returns a stable identifier used in logs and memory records.
	Name() string

	// Execute runs the agent's primary task and returns a structured result.
	// Implementations must propagate ctx cancellation and never swallow errors.
	Execute(ctx context.Context, input AgentInput) (AgentOutput, error)
}

// AgentInput is the typed envelope passed into every agent.
type AgentInput struct {
	// Task is a human-readable label for the operation (e.g. "generate_seo_content").
	Task string

	// Payload carries the concrete input struct for this task.
	// The agent is responsible for type-asserting it to the expected type.
	Payload interface{}
}

// AgentOutput is the typed envelope returned by every agent.
type AgentOutput struct {
	// AgentName mirrors Agent.Name() for tracing purposes.
	AgentName string

	// Success indicates whether the agent completed without error.
	Success bool

	// Data carries the concrete result struct.
	// Callers type-assert this to the expected output type.
	Data interface{}

	// Error holds a human-readable error string when Success is false.
	// The error returned by Execute() is the authoritative Go error;
	// this field is provided for logging and memory persistence.
	Error string
}
