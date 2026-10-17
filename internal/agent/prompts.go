package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/BiodigitalJaz/go-agent-spawner/internal/llm"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/settings"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/store"
)

type plan struct {
	Strategy string    `json:"strategy"` // direct | split
	Reason   string    `json:"reason"`
	Subtasks []subtask `json:"subtasks"`
}

type subtask struct {
	Title   string `json:"title"`
	Prompt  string `json:"prompt"`
	Complex bool   `json:"complex"`
}

func planSchema(maxChildren int) *llm.Schema {
	schema := fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "strategy": {"type": "string", "enum": ["direct", "split"]},
    "reason": {"type": "string"},
    "subtasks": {
      "type": "array",
      "maxItems": %d,
      "items": {
        "type": "object",
        "properties": {
          "title": {"type": "string"},
          "prompt": {"type": "string"},
          "complex": {"type": "boolean"}
        },
        "required": ["title", "prompt", "complex"],
        "additionalProperties": false
      }
    }
  },
  "required": ["strategy", "reason", "subtasks"],
  "additionalProperties": false
}`, maxChildren)
	return &llm.Schema{Name: "plan", Schema: json.RawMessage(schema)}
}

// planRules is appended to the planner's editable directive. These rules are
// what the split/join machinery depends on, so they are not user-editable.
func planRules(maxChildren int, canNest bool) string {
	nest := "Set complex=false on every subtask."
	if canNest {
		nest = "Set complex=true only on a subtask that is itself large enough to merit splitting again."
	}
	return fmt.Sprintf(`Rules for subtasks:
- At most %d subtasks, and at least 2 if you split.
- Each prompt must be fully self-contained. Workers cannot see the original task or each other's work.
- %s
- Results will be merged by a separate synthesis step, so do not add a "combine results" subtask.

When the strategy is "direct", return an empty subtasks array.`, maxChildren, nest)
}

// withReference puts a tree's reference material before the task. Every
// agent (planner, workers, merge) receives it, so subtasks can rely on it.
func withReference(set *settings.Settings, task string) string {
	if strings.TrimSpace(set.Context) == "" {
		return task
	}
	return "Reference material provided with this task (your only source for its content):\n" +
		"--- BEGIN REFERENCE ---\n" + set.Context + "\n--- END REFERENCE ---\n\n" + task
}

func planMessages(set *settings.Settings, node *store.Node, canNest bool) []llm.Message {
	rules := planRules(set.MaxChildren, canNest)
	if strings.TrimSpace(set.Context) != "" {
		rules += "\n\nEvery worker also receives the reference material below, so subtask prompts may refer to it."
	}
	return []llm.Message{
		{Role: "system", Content: strings.TrimSpace(set.Planner.Directive) + "\n\n" + rules},
		{Role: "user", Content: withReference(set, node.Prompt)},
	}
}

func execMessages(set *settings.Settings, node *store.Node) []llm.Message {
	return []llm.Message{
		{Role: "system", Content: set.Worker.Directive},
		{Role: "user", Content: withReference(set, node.Prompt)},
	}
}

// childItems turns a node's children into merge inputs. Token counts come
// from each child's recorded completion tokens when available (exact), else
// a conservative character estimate.
func childItems(children []*store.Node) []mergeItem {
	items := make([]mergeItem, 0, len(children))
	for _, c := range children {
		it := mergeItem{Title: c.Title}
		switch {
		case c.Status == store.StatusDone && c.Output != nil:
			it.Text = *c.Output
		case c.Error != nil:
			it.Text = fmt.Sprintf("(this subtask failed: %s)", *c.Error)
		default:
			it.Text = fmt.Sprintf("(no result, status %s)", c.Status)
		}
		it.Tokens = estimateTokens(it.Text)
		var st struct {
			Tokens int `json:"tokens"`
		}
		if c.Output != nil && len(c.Stats) > 0 && json.Unmarshal(c.Stats, &st) == nil && st.Tokens > 0 {
			it.Tokens = st.Tokens
		}
		items = append(items, it)
	}
	return items
}

func reduceMessages(set *settings.Settings, node *store.Node, items []mergeItem) []llm.Message {
	return []llm.Message{
		{Role: "system", Content: set.Reducer.Directive},
		{Role: "user", Content: withReference(set, fmt.Sprintf("Original task:\n%s\n\nResults from parallel workers:\n%s", node.Prompt, itemsBlock(items)))},
	}
}

// consolidateMessages asks for a lossless condensation of a batch of results,
// used when all results together would not fit one merge.
func consolidateMessages(node *store.Node, items []mergeItem) []llm.Message {
	return []llm.Message{
		{Role: "system", Content: "You consolidate partial results produced by parallel worker agents into one combined partial result. " +
			"Preserve every concrete fact, name, number, list item and code identifier. Remove only exact duplicates. " +
			"Do not summarize details away and do not add commentary. Another step will merge your output with other groups."},
		{Role: "user", Content: fmt.Sprintf("Original task (for context only):\n%s\n\nPartial results to consolidate:\n%s", node.Prompt, itemsBlock(items))},
	}
}
