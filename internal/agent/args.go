// Package agent implements the task-tree worker: plan, split into parallel
// children, execute, and reduce, with River providing durability and
// distribution and the backend pool providing adaptive GPU scheduling.
package agent

import (
	"github.com/google/uuid"
	"github.com/riverqueue/river"
)

type Role string

const (
	// RolePlan decides whether a node is answered directly or split.
	RolePlan Role = "plan"
	// RoleExec answers a node's prompt directly.
	RoleExec Role = "exec"
	// RoleReduce synthesizes a split node's answer from its children.
	RoleReduce Role = "reduce"
)

const Kind = "agent_task"

// TaskArgs is deliberately small: prompts and outputs live in agent_node and
// are loaded by ID, since args are stored as jsonb on every job row.
type TaskArgs struct {
	TreeID   uuid.UUID `json:"tree_id" river:"unique"`
	NodeID   uuid.UUID `json:"node_id" river:"unique"`
	Role     Role      `json:"role" river:"unique"`
	Model    string    `json:"model"`
	Priority int       `json:"priority"`
}

func (TaskArgs) Kind() string { return Kind }

// InsertOpts routes the job to its model's queue. Uniqueness on (node, role)
// makes every insert idempotent, so a reduce job can never be queued twice
// even if a sibling completion is retried.
func (a TaskArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       QueueName(a.Model),
		Priority:    min(max(a.Priority, 1), 4),
		MaxAttempts: 4,
		UniqueOpts:  river.UniqueOpts{ByArgs: true},
	}
}

// QueueName maps a model routing name to its River queue.
func QueueName(model string) string { return "llm_" + model }
