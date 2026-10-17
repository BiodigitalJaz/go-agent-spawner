// Package mcpserver exposes the agent spawner as Model Context Protocol tools,
// so coding agents and chat clients can hand parallel work to the pool: run a
// task, process a long document in chunks, or analyze many local files with
// one agent per file. The tools call the spawner's HTTP API; the same server
// runs over stdio (`spawner mcp`, with local-file tools) and over Streamable
// HTTP at /mcp on the API (the caller's own bearer token is forwarded).
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Options struct {
	// DefaultAuth is the Authorization header used when the MCP request
	// carries none (stdio). Over HTTP the caller's header is used.
	DefaultAuth string
	// ConsoleURL builds links to runs in the web console.
	ConsoleURL string
	// LocalFiles enables analyze_files, which reads files from this
	// machine. Only for stdio, where the server runs beside the client.
	LocalFiles bool
	// Root is the directory relative paths resolve against (default: cwd).
	Root    string
	Version string
}

type server struct {
	api  *Client
	opts Options
}

const instructions = `Agent Spawner runs work on a pool of local LLM agents in parallel and merges the results.
Use it to offload bulk reading or per-item work: long documents, many files, logs.
- The agents cannot browse or read files: they see only what you send. Put shared reference material (a page under review, rules the answer must respect) in "context"; links in the prompt are fetched into it by default.
- run_task: a prompt, optionally with a long "document" (split into chunks automatically) or explicit "parts" (one agent each).
- analyze_files: one agent per local file, e.g. "list every TODO and its function" across a package.
- merge: "list" for extraction and lists (fastest; distinct lines), "concat" for per-item outputs such as per-file reviews (verbatim), "llm" when one synthesized answer is needed.
Results come from smaller local models: verify important claims against the source.`

// New builds the MCP server with its tools.
func New(api *Client, opts Options) *mcp.Server {
	s := &server{api: api, opts: opts}
	srv := mcp.NewServer(&mcp.Implementation{Name: "agent-spawner", Title: "Agent Spawner", Version: opts.Version},
		&mcp.ServerOptions{Instructions: instructions})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "run_task",
		Title:       "Run a task on the agent pool",
		Description: "Run a prompt on the local agent pool and return the merged result. Agents cannot browse: give them what they need. Shared reference material that every agent should see (a page under review, rules to respect) goes in `context`; a long input to split goes in `document` (chunked automatically: one context up to about 6k tokens, otherwise 4+ chunks); separate items go in `parts` (one agent per part). Web links in the prompt are fetched into the context unless fetch_urls is false. Waits for the result by default, reporting progress.",
		Annotations: &mcp.ToolAnnotations{Title: "Run task", OpenWorldHint: boolPtr(false)},
	}, s.runTask)

	if opts.LocalFiles {
		mcp.AddTool(srv, &mcp.Tool{
			Name:        "analyze_files",
			Title:       "Analyze local files in parallel",
			Description: "Read local files (paths, directories or glob patterns, relative to the working directory) and give each file to its own agent with the same instruction, then merge. Directories are walked recursively, skipping hidden, vendor and build directories. Binary and very large files are skipped and reported.",
			Annotations: &mcp.ToolAnnotations{Title: "Analyze files", ReadOnlyHint: false, OpenWorldHint: boolPtr(false)},
		}, s.analyzeFiles)
	}

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_task",
		Title:       "Get a task's status and result",
		Description: "Get the status and result of a task started with run_task or analyze_files, optionally with each part's own output.",
		Annotations: &mcp.ToolAnnotations{Title: "Get task", ReadOnlyHint: true, OpenWorldHint: boolPtr(false)},
	}, s.getTask)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "cancel_task",
		Title:       "Cancel a running task",
		Description: "Cancel a task: running agents stop and queued ones never start.",
		Annotations: &mcp.ToolAnnotations{Title: "Cancel task", DestructiveHint: boolPtr(true), IdempotentHint: true, OpenWorldHint: boolPtr(false)},
	}, s.cancelTask)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_models",
		Title:       "List models and capacity",
		Description: "List the models the pool can route to, which model each agent role uses, and how many workers are online.",
		Annotations: &mcp.ToolAnnotations{Title: "List models", ReadOnlyHint: true, OpenWorldHint: boolPtr(false)},
	}, s.listModels)
	return srv
}

func boolPtr(b bool) *bool { return &b }

func (s *server) auth(req *mcp.CallToolRequest) string {
	if req != nil && req.Extra != nil && req.Extra.Header != nil {
		if h := req.Extra.Header.Get("Authorization"); h != "" {
			return h
		}
	}
	return s.opts.DefaultAuth
}

// ---- run_task / analyze_files ----

type RunInput struct {
	Prompt         string `json:"prompt" jsonschema:"the task, or the instruction applied to the document or each part"`
	Document       string `json:"document,omitempty" jsonschema:"a long input to split into chunks handled in parallel"`
	Parts          []Part `json:"parts,omitempty" jsonschema:"inputs given one agent each, such as files; titles name them in the result"`
	Chunks         int    `json:"chunks,omitempty" jsonschema:"number of chunks for document; 0 picks automatically (1 up to about 6k tokens, else 4 or more)"`
	Context        string `json:"context,omitempty" jsonschema:"reference material every agent receives (planner, workers and merge): a page you fetched, rules the answer must respect. Up to about 24,000 characters"`
	Check          *bool  `json:"check,omitempty" jsonschema:"when context is given, check the final answer against it and remove or fix conflicting recommendations (default: the server setting, normally on)"`
	FetchURLs      *bool  `json:"fetch_urls,omitempty" jsonschema:"fetch the web pages linked in the prompt into the context (default true). Set false if you pass the page content yourself"`
	PartMaxWords   int    `json:"part_max_words,omitempty" jsonschema:"ask each part's agent for an answer under this many words; the final merge keeps full length. For summaries with merge llm, 100-150 roughly halves the time. Avoid for extraction (lists of every X), where it can drop items"`
	MaxPartTokens  int    `json:"max_part_tokens,omitempty" jsonschema:"parts larger than this many tokens are split into sections, one agent each (default 16000; -1 keeps parts whole)"`
	Merge          string `json:"merge,omitempty" jsonschema:"how results are combined: list (distinct lines, for extraction), concat (verbatim, per-item outputs) or llm (synthesized answer); default is the server setting" enum:"list,concat,llm"`
	Model          string `json:"model,omitempty" jsonschema:"worker model name (see list_models); default is the server setting"`
	MaxDepth       *int   `json:"max_depth,omitempty" jsonschema:"for prompts without document or parts: how deep the planner may split (0 = one agent answers)"`
	Priority       int    `json:"priority,omitempty" jsonschema:"1 (highest) to 4; default 1"`
	Wait           *bool  `json:"wait,omitempty" jsonschema:"wait for the result (default true); false returns the task id at once"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"how long to wait before returning the task id (default 600)"`
}

type FilesInput struct {
	Paths          []string `json:"paths" jsonschema:"files, directories or glob patterns, relative to the working directory"`
	Instruction    string   `json:"instruction" jsonschema:"what each agent does with its file, e.g. list every exported function without a doc comment"`
	Extensions     []string `json:"extensions,omitempty" jsonschema:"only these file extensions, e.g. [\"go\", \"ts\"]"`
	Merge          string   `json:"merge,omitempty" jsonschema:"concat (default: each file's output verbatim), list (distinct lines) or llm (one synthesized answer)" enum:"concat,list,llm"`
	MaxFiles       int      `json:"max_files,omitempty" jsonschema:"at most this many files (default and maximum 64)"`
	MaxFileBytes   int      `json:"max_file_bytes,omitempty" jsonschema:"skip files larger than this (default 400000 bytes)"`
	Context        string   `json:"context,omitempty" jsonschema:"reference material every file's agent receives, e.g. coding standards or rules to check against"`
	Check          *bool    `json:"check,omitempty" jsonschema:"with context and merge llm, check the final answer against it (default: the server setting, normally on)"`
	PartMaxWords   int      `json:"part_max_words,omitempty" jsonschema:"ask each part's agent for an answer under this many words; the final merge keeps full length. For summaries with merge llm, 100-150 roughly halves the time. Avoid for extraction (lists of every X), where it can drop items"`
	MaxPartTokens  int      `json:"max_part_tokens,omitempty" jsonschema:"files larger than this many tokens are split into sections, one agent each (default 16000; -1 keeps files whole)"`
	Model          string   `json:"model,omitempty" jsonschema:"worker model name; default is the server setting"`
	Wait           *bool    `json:"wait,omitempty" jsonschema:"wait for the result (default true)"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty" jsonschema:"how long to wait before returning the task id (default 600)"`
}

type PartResult struct {
	Title  string `json:"title"`
	Status string `json:"status"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

type RunOutput struct {
	TaskID      string       `json:"task_id"`
	Status      string       `json:"status" jsonschema:"done, failed, cancelled, or running when the wait timed out"`
	Result      string       `json:"result,omitempty"`
	Error       string       `json:"error,omitempty"`
	Agents      int          `json:"agents"`
	FailedParts []PartResult `json:"failed_parts,omitempty"`
	Skipped     []string     `json:"skipped_files,omitempty"`
	Chunks      int          `json:"chunks,omitempty"`
	Seconds     float64      `json:"seconds"`
	ConsoleURL  string       `json:"console_url,omitempty"`
}

// AutoChunks applies the measured rule of thumb: one context is as accurate
// and faster up to about 6k tokens; above that 4 chunks were fastest and
// fully accurate up to 65k tokens on an 8-slot server.
func AutoChunks(doc string) int {
	t := len(doc) / 4
	switch {
	case t <= 6000:
		return 1
	case t <= 30000:
		return 4
	default:
		return min(16, (t+7499)/7500)
	}
}

func (s *server) runTask(ctx context.Context, req *mcp.CallToolRequest, in RunInput) (*mcp.CallToolResult, RunOutput, error) {
	if strings.TrimSpace(in.Prompt) == "" {
		return nil, RunOutput{}, errors.New("prompt is required")
	}
	body := CreateTree{Prompt: in.Prompt, Priority: in.Priority, Model: in.Model, MaxDepth: in.MaxDepth, Merge: in.Merge, Parts: in.Parts, MaxPartTokens: in.MaxPartTokens, PartMaxWords: in.PartMaxWords,
		Context: in.Context, FetchURLs: in.FetchURLs == nil || *in.FetchURLs, Check: in.Check}
	chunks := 0
	if len(in.Parts) == 0 && in.Document != "" {
		chunks = in.Chunks
		if chunks <= 0 {
			chunks = AutoChunks(in.Document)
		}
		body.Document, body.Chunks = in.Document, chunks
	}
	return s.start(ctx, req, body, chunks, nil, in.Wait, in.TimeoutSeconds)
}

func (s *server) analyzeFiles(ctx context.Context, req *mcp.CallToolRequest, in FilesInput) (*mcp.CallToolResult, RunOutput, error) {
	if len(in.Paths) == 0 || strings.TrimSpace(in.Instruction) == "" {
		return nil, RunOutput{}, errors.New("paths and instruction are required")
	}
	root := s.opts.Root
	if root == "" {
		root, _ = os.Getwd()
	}
	got, err := collectFiles(root, in.Paths, in.Extensions, in.MaxFiles, in.MaxFileBytes)
	if err != nil {
		return nil, RunOutput{}, err
	}
	if len(got.Parts) == 0 {
		return nil, RunOutput{Skipped: got.Skipped}, fmt.Errorf("no readable text files matched (%d skipped)", len(got.Skipped))
	}
	merge := in.Merge
	if merge == "" {
		merge = "concat"
	}
	body := CreateTree{Prompt: in.Instruction, Model: in.Model, Merge: merge, Parts: got.Parts, MaxPartTokens: in.MaxPartTokens, PartMaxWords: in.PartMaxWords, Context: in.Context, Check: in.Check}
	return s.start(ctx, req, body, len(got.Parts), got.Skipped, in.Wait, in.TimeoutSeconds)
}

func (s *server) start(ctx context.Context, req *mcp.CallToolRequest, body CreateTree, chunks int, skipped []string, wait *bool, timeoutSec int) (*mcp.CallToolResult, RunOutput, error) {
	auth := s.auth(req)
	began := time.Now()
	id, err := s.api.CreateTree(ctx, auth, body)
	if err != nil {
		return nil, RunOutput{}, err
	}
	out := RunOutput{TaskID: id, Status: "running", Chunks: chunks, Skipped: skipped, ConsoleURL: s.consoleLink(id)}
	if wait != nil && !*wait {
		return textResult(fmt.Sprintf("Started task %s. Call get_task to fetch the result.", id), false), out, nil
	}
	if timeoutSec <= 0 {
		timeoutSec = 600
	}
	wctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()
	s.follow(wctx, req, auth, id)
	if ctx.Err() != nil {
		return nil, out, ctx.Err() // the caller gave up
	}
	return s.report(ctx, auth, id, out, began)
}

// follow relays progress (agents finished / agents planned) until the task
// ends or ctx expires.
func (s *server) follow(ctx context.Context, req *mcp.CallToolRequest, auth, id string) {
	token := req.Params.GetProgressToken()
	total, done := 1.0, 0.0
	notify := func(msg string) {
		if token != nil && req.Session != nil {
			_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: token, Progress: done, Total: total, Message: msg})
		}
	}
	_ = s.api.Follow(ctx, auth, id, func(e Event) bool {
		switch e.Type {
		case "node.split":
			total += float64(len(e.Children))
			notify(fmt.Sprintf("split into %d agents", len(e.Children)))
		case "node.done", "node.failed":
			done++
			what := e.Title
			if what == "" {
				what = e.Role
			}
			notify(fmt.Sprintf("%.0f/%.0f agents finished (%s %s)", done, total, strings.TrimPrefix(e.Type, "node."), what))
		}
		return true
	})
}

func (s *server) report(ctx context.Context, auth, id string, out RunOutput, began time.Time) (*mcp.CallToolResult, RunOutput, error) {
	t, err := s.api.GetTree(ctx, auth, id)
	if err != nil {
		return nil, out, err
	}
	out.Seconds = time.Since(began).Round(100 * time.Millisecond).Seconds()
	out.Agents = len(t.Nodes)
	out.Status = t.Status
	for _, n := range t.Nodes {
		if n.ParentID == nil {
			if n.Output != nil {
				out.Result = *n.Output
			}
			if n.Error != nil {
				out.Error = *n.Error
			}
			continue
		}
		if n.Status == "failed" || n.Status == "cancelled" {
			out.FailedParts = append(out.FailedParts, PartResult{Title: n.Title, Status: n.Status, Error: deref(n.Error)})
		}
	}
	switch out.Status {
	case "done":
		return textResult(out.Result+footer(out), false), out, nil
	case "failed", "cancelled":
		return textResult(fmt.Sprintf("Task %s %s: %s%s", id, out.Status, out.Error, footer(out)), true), out, nil
	default:
		out.Status = "running"
		return textResult(fmt.Sprintf("Task %s is still running after the wait. Call get_task with this id for the result.%s", id, footer(out)), false), out, nil
	}
}

func footer(o RunOutput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n\n---\ntask %s · %s · %d agents · %.1fs", o.TaskID, o.Status, o.Agents, o.Seconds)
	if len(o.FailedParts) > 0 {
		names := make([]string, len(o.FailedParts))
		for i, p := range o.FailedParts {
			names[i] = p.Title
		}
		fmt.Fprintf(&b, "\nfailed parts: %s", strings.Join(names, ", "))
	}
	if len(o.Skipped) > 0 {
		fmt.Fprintf(&b, "\nskipped files: %s", strings.Join(o.Skipped, "; "))
	}
	if o.ConsoleURL != "" {
		fmt.Fprintf(&b, "\nconsole: %s", o.ConsoleURL)
	}
	return b.String()
}

func (s *server) consoleLink(id string) string {
	if s.opts.ConsoleURL == "" {
		return ""
	}
	return strings.TrimSuffix(s.opts.ConsoleURL, "/") + "/#/runs/" + id
}

func textResult(text string, isErr bool) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, IsError: isErr}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ---- get_task / cancel_task / list_models ----

type TaskInput struct {
	TaskID          string `json:"task_id" jsonschema:"the id returned by run_task or analyze_files"`
	IncludeParts    bool   `json:"include_parts,omitempty" jsonschema:"also return each part's own output"`
	MaxCharsPerPart int    `json:"max_chars_per_part,omitempty" jsonschema:"truncate each part's output to this many characters (default 4000)"`
}

type TaskOutput struct {
	TaskID     string         `json:"task_id"`
	Status     string         `json:"status"`
	Prompt     string         `json:"prompt"`
	Result     string         `json:"result,omitempty"`
	Error      string         `json:"error,omitempty"`
	Agents     int            `json:"agents"`
	Counts     map[string]int `json:"counts"`
	Parts      []PartResult   `json:"parts,omitempty"`
	ConsoleURL string         `json:"console_url,omitempty"`
}

func (s *server) getTask(ctx context.Context, req *mcp.CallToolRequest, in TaskInput) (*mcp.CallToolResult, TaskOutput, error) {
	t, err := s.api.GetTree(ctx, s.auth(req), in.TaskID)
	if err != nil {
		return nil, TaskOutput{}, err
	}
	limit := in.MaxCharsPerPart
	if limit <= 0 {
		limit = 4000
	}
	out := TaskOutput{TaskID: in.TaskID, Status: t.Status, Prompt: clip(t.Prompt, 500), Agents: len(t.Nodes), Counts: map[string]int{}, ConsoleURL: s.consoleLink(in.TaskID)}
	for _, n := range t.Nodes {
		out.Counts[n.Status]++
		if n.ParentID == nil {
			out.Result, out.Error = deref(n.Output), deref(n.Error)
			continue
		}
		if in.IncludeParts {
			out.Parts = append(out.Parts, PartResult{Title: n.Title, Status: n.Status, Output: clip(deref(n.Output), limit), Error: deref(n.Error)})
		}
	}
	text := fmt.Sprintf("Task %s: %s (%d agents).", in.TaskID, out.Status, out.Agents)
	if out.Result != "" {
		text = out.Result + "\n\n---\n" + text
	}
	return textResult(text, false), out, nil
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + " […]"
	}
	return s
}

type CancelOutput struct {
	TaskID        string `json:"task_id"`
	CancelledJobs int    `json:"cancelled_jobs"`
}

func (s *server) cancelTask(ctx context.Context, req *mcp.CallToolRequest, in TaskInput) (*mcp.CallToolResult, CancelOutput, error) {
	n, err := s.api.CancelTree(ctx, s.auth(req), in.TaskID)
	if err != nil {
		return nil, CancelOutput{}, err
	}
	return textResult(fmt.Sprintf("Cancelled task %s (%d jobs stopped).", in.TaskID, n), false), CancelOutput{TaskID: in.TaskID, CancelledJobs: n}, nil
}

type ModelInfo struct {
	Name          string `json:"name"`
	Model         string `json:"model"`
	Source        string `json:"source"`
	ContextLength int    `json:"context_length,omitempty"`
	Slots         int    `json:"slots,omitempty"`
	SharedContext bool   `json:"shared_context,omitempty"`
	Reasoning     string `json:"reasoning,omitempty"`
}

type ModelsOutput struct {
	Models  []ModelInfo       `json:"models"`
	Roles   map[string]string `json:"roles"`
	Merge   string            `json:"default_merge"`
	Workers int               `json:"workers_online"`
}

type NoInput struct{}

func (s *server) listModels(ctx context.Context, req *mcp.CallToolRequest, _ NoInput) (*mcp.CallToolResult, ModelsOutput, error) {
	auth := s.auth(req)
	rawModels, err := s.api.Models(ctx, auth)
	if err != nil {
		return nil, ModelsOutput{}, err
	}
	rawSettings, err := s.api.Settings(ctx, auth)
	if err != nil {
		return nil, ModelsOutput{}, err
	}
	var m struct {
		Models []struct {
			ModelInfo
			Extra map[string]any `json:"extra"`
		} `json:"models"`
		Workers []json.RawMessage `json:"workers"`
	}
	var st struct {
		Settings struct {
			Planner, Worker, Reducer struct {
				Model string `json:"model"`
			}
			Merge string `json:"merge"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(rawModels, &m); err != nil {
		return nil, ModelsOutput{}, err
	}
	if err := json.Unmarshal(rawSettings, &st); err != nil {
		return nil, ModelsOutput{}, err
	}
	out := ModelsOutput{Workers: len(m.Workers), Merge: st.Settings.Merge,
		Roles: map[string]string{"planner": st.Settings.Planner.Model, "worker": st.Settings.Worker.Model, "reducer": st.Settings.Reducer.Model}}
	if out.Merge == "" {
		out.Merge = "llm"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d workers online. Roles: planner=%s, worker=%s, reducer=%s; default merge %s.\n", out.Workers, out.Roles["planner"], out.Roles["worker"], out.Roles["reducer"], out.Merge)
	for _, x := range m.Models {
		info := x.ModelInfo
		if r, ok := x.Extra["reasoning_effort"].(string); ok {
			info.Reasoning = map[bool]string{true: "off", false: r}[r == "none"]
		}
		out.Models = append(out.Models, info)
		fmt.Fprintf(&b, "- %s (%s, %s)", info.Name, info.Model, info.Source)
		if info.ContextLength > 0 {
			fmt.Fprintf(&b, ", context %d", info.ContextLength)
		}
		if info.Slots > 0 {
			fmt.Fprintf(&b, ", %d slots", info.Slots)
		}
		if info.Reasoning != "" {
			fmt.Fprintf(&b, ", reasoning %s", info.Reasoning)
		}
		b.WriteString("\n")
	}
	return textResult(b.String(), false), out, nil
}
