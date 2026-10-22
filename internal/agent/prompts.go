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
	return "Reference material provided with this task. Use it as your source for what it covers; for anything it does not cover, use your own knowledge and say so. Never claim it says something it does not. Something it does not mention is not thereby forbidden: only call something prohibited when the reference says so in plain words:\n" +
		"--- BEGIN REFERENCE ---\n" + set.Context + "\n--- END REFERENCE ---\n\n" + task
}

// conversationBlock renders the conversation before a root's prompt as
// text, for agents that take it as context rather than as chat turns.
func conversationBlock(set *settings.Settings, node *store.Node) string {
	if node.ParentID != nil || len(set.History) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Conversation so far (the latest message follows):\n")
	for _, t := range set.History {
		who := "User"
		if t.Role == "assistant" {
			who = "Assistant"
		}
		fmt.Fprintf(&b, "%s: %s\n\n", who, t.Content)
	}
	b.WriteString("Latest message:\n")
	return b.String()
}

func planMessages(set *settings.Settings, node *store.Node, canNest bool) []llm.Message {
	rules := planRules(set.MaxChildren, canNest)
	if strings.TrimSpace(set.Context) != "" {
		rules += "\n\nEvery worker also receives the reference material below, so subtask prompts may refer to it." +
			" When the reference has both pages linked in the task and guidelines to respect, each subtask reviews the linked pages against the guidelines and proposes concrete changes to them; subtasks never evaluate, praise or restate the guidelines themselves." +
			" Web search results, when present, are background only: never mistake them for the pages linked in the task."
	}
	return []llm.Message{
		{Role: "system", Content: strings.TrimSpace(set.Planner.Directive) + "\n\n" + rules},
		{Role: "user", Content: withReference(set, conversationBlock(set, node)+node.Prompt)},
	}
}

// execMessages gives a chat reply's root agent the earlier conversation as
// real turns, then the latest message.
func execMessages(set *settings.Settings, node *store.Node) []llm.Message {
	system := set.Worker.Directive
	if node.ParentID == nil && len(set.History) > 0 {
		// Gemma 4 repeated its previous answer before answering a follow-up
		// when the reference came with the latest message.
		system += "\n\nThis is a continuing conversation. Reply to the latest message only; do not repeat or summarize your earlier answers unless asked."
	}
	msgs := []llm.Message{{Role: "system", Content: system}}
	if node.ParentID == nil {
		for _, t := range set.History {
			role := "user"
			if t.Role == "assistant" {
				role = "assistant"
			}
			msgs = append(msgs, llm.Message{Role: role, Content: t.Content})
		}
	}
	return append(msgs, llm.Message{Role: "user", Content: withReference(set, node.Prompt)})
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
		{Role: "user", Content: withReference(set, fmt.Sprintf("Original task:\n%s%s\n\nResults from parallel workers:\n%s", conversationBlock(set, node), node.Prompt, itemsBlock(items)))},
	}
}

// checkDirective drives the rules check of a final answer.
const checkDirective = `You check an answer against reference material that states facts and rules (what to do and what not to do).

Find every statement or recommendation in the answer that conflicts with a rule or contradicts a fact in the reference, or that claims something the reference does not support. Then return the answer with each of those fixed so it complies, or removed if it cannot be fixed. Keep everything else exactly as it was, including formatting.

After the answer, add a section headed "### Rules check" that lists each change in one line: what was changed or removed, and the rule or fact it conflicted with (quote it briefly). If nothing conflicts, keep the answer unchanged and write "No conflicts with the reference found." under that heading. Respond with the checked answer and that section only.`

func checkMessages(set *settings.Settings, node *store.Node, answer string) []llm.Message {
	return []llm.Message{
		{Role: "system", Content: checkDirective},
		{Role: "user", Content: withReference(set, fmt.Sprintf("Original task:\n%s\n\nAnswer to check:\n--- BEGIN ANSWER ---\n%s\n--- END ANSWER ---", node.Prompt, answer))},
	}
}

// sourceCheckDirective drives the review of an answer written from web
// search results. Small models mixed sourced facts with invented ones (a
// "$20 crossbow permit") and read a rule's absence as a ban.
const sourceCheckDirective = `You review an answer against the web pages it was written from (the reference). Your job is to keep the answer helpful while removing what the pages do not support.

Go through the answer and look at the facts that depend on place or time: fees, prices, dates, seasons, deadlines, limits, equipment rules, required licenses or permits, and statements that something is legal, illegal, required or not allowed.
- If the reference states it, keep it.
- If the reference does not state it, remove it, or keep it with "(unverified, not in the sources)" right after it when it is useful general knowledge.
- If the answer says something is illegal, not allowed or not possible but the reference never says so in plain words, rewrite that part to give the practical help and say which rule to confirm and where (name the agency or page from the reference).
- If the reference points to another page for a rule ("see Big Game Regulations") and that page is not in the reference, say the rule is on that page.

Leave general know-how alone even when the reference does not cover it: techniques, steps, recipes, cooking times and temperatures, and tips stay as they are. Never remove such content. Do not add new facts, and keep the formatting, headings and source links. Respond with the reviewed answer only, with no notes about the review.`

func sourceCheckMessages(set *settings.Settings, node *store.Node, answer string) []llm.Message {
	return []llm.Message{
		{Role: "system", Content: sourceCheckDirective},
		{Role: "user", Content: withReference(set, fmt.Sprintf("Question:\n%s\n\nAnswer to review:\n--- BEGIN ANSWER ---\n%s\n--- END ANSWER ---", node.Prompt, answer))},
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
