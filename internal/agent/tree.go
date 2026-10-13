package agent

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/BiodigitalJaz/go-agent-spawner/internal/events"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/settings"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/store"
)

type TreeRequest struct {
	Prompt   string
	Priority int // 1 (highest) to 4
	// Settings is the full configuration the tree runs with; callers apply
	// any per-request overrides before passing it in.
	Settings settings.Settings
	// Document, when set, is input the task operates on. With Chunks > 1 it
	// is split in code into that many parts, each handled by its own agent
	// and merged by the reducer (no planner: an LLM would have to re-type the
	// document). Otherwise the whole document goes into one agent's context.
	Document string
	Chunks   int
	// Parts, when set, are given pieces of input (files, records), one agent
	// each, merged like document chunks. They take precedence over Document.
	Parts []Part
	// MaxPartTokens splits given parts larger than this into sections, one
	// agent each. 0 means DefaultMaxPartTokens; negative disables splitting.
	MaxPartTokens int
}

// Part is one explicitly given piece of input, such as a file.
type Part struct {
	Title string `json:"title"`
	Text  string `json:"text"`
}

// StartedTree is what StartTree created; Children is set for chunked trees.
type StartedTree struct {
	Root     *store.Node
	Children []*store.Node
}

// StartTree snapshots the settings, creates the root node and inserts its
// first job(s), all in one transaction.
func StartTree(ctx context.Context, db *pgxpool.Pool, client *river.Client[pgx.Tx], req TreeRequest) (*StartedTree, error) {
	if len(req.Parts) > 0 {
		parts, err := expandParts(req.Parts, req.MaxPartTokens)
		if err != nil {
			return nil, err
		}
		req.Parts = parts
		return startChunked(ctx, db, client, req)
	}
	if req.Document != "" && req.Chunks > 1 {
		parts := splitDocument(req.Document, req.Chunks)
		req.Parts = make([]Part, len(parts))
		for i, p := range parts {
			req.Parts[i] = Part{Title: fmt.Sprintf("Part %d/%d", i+1, len(parts)), Text: p}
		}
		return startChunked(ctx, db, client, req)
	}
	if req.Document != "" {
		// Single-context mode: one agent sees the whole document.
		req.Prompt = documentPrompt(req.Prompt, req.Document, 0, 0)
		req.Settings.MaxDepth = 0
	}
	root, err := startPlanned(ctx, db, client, req)
	if err != nil {
		return nil, err
	}
	return &StartedTree{Root: root}, nil
}

func documentPrompt(instruction, doc string, part, of int) string {
	if of == 0 {
		return fmt.Sprintf("%s\n\n--- DOCUMENT ---\n%s\n--- END DOCUMENT ---", instruction, doc)
	}
	return fmt.Sprintf("%s\n\nYou are given part %d of %d of a larger document. Work only from this part; other parts are handled by other agents, so do not speculate about them.\n\n--- DOCUMENT PART %d/%d ---\n%s\n--- END PART ---",
		instruction, part, of, part, of, doc)
}

// partPrompt frames one given part (a file, a record) for its agent.
func partPrompt(instruction string, p Part, i, n int) string {
	return fmt.Sprintf("%s\n\nYou are given item %d of %d: %s. Work only from this item; other items are handled by other agents, so do not speculate about them.\n\n--- BEGIN %s ---\n%s\n--- END %s ---",
		instruction, i, n, p.Title, p.Title, p.Text, p.Title)
}

// startChunked fans out one exec agent per part (document chunks or given
// parts); the root waits for them and its reduce merges the results.
func startChunked(ctx context.Context, db *pgxpool.Pool, client *river.Client[pgx.Tx], req TreeRequest) (*StartedTree, error) {
	set := req.Settings
	id := uuid.New()
	parts := req.Parts
	root := &store.Node{ID: id, TreeID: id, Prompt: req.Prompt, Model: set.Worker.Model, Role: string(RolePlan), Status: store.StatusQueued}
	children := make([]*store.Node, len(parts))
	params := make([]river.InsertManyParams, 0, len(parts))
	chunked := req.Document != "" // document chunks keep the "part i of n of a larger document" framing
	for i, part := range parts {
		prompt := partPrompt(req.Prompt, part, i+1, len(parts))
		if chunked {
			prompt = documentPrompt(req.Prompt, part.Text, i+1, len(parts))
		}
		c := &store.Node{
			ID: uuid.New(), TreeID: id, ParentID: &root.ID, Idx: i, Depth: 1,
			Title:  part.Title,
			Prompt: prompt,
			Model:  set.Worker.Model, Role: string(RoleExec), Status: store.StatusQueued,
		}
		children[i] = c
	}
	// Jobs of equal priority run in insertion order, so insert the largest
	// parts first. Node indexes keep the given order for the merge.
	for _, i := range largestFirst(parts) {
		params = append(params, river.InsertManyParams{Args: TaskArgs{TreeID: id, NodeID: children[i].ID, Role: RoleExec, Model: set.Worker.Model, Priority: req.Priority}})
	}
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if err := settings.SnapshotTree(ctx, tx, id, req.Prompt, set); err != nil {
			return err
		}
		if err := store.InsertNodes(ctx, tx, root); err != nil {
			return err
		}
		if err := store.MarkRunning(ctx, tx, id); err != nil {
			return err
		}
		if err := store.Split(ctx, tx, id, children); err != nil {
			return err
		}
		_, err := client.InsertManyTx(ctx, tx, params)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &StartedTree{Root: root, Children: children}, nil
}

func startPlanned(ctx context.Context, db *pgxpool.Pool, client *river.Client[pgx.Tx], req TreeRequest) (*store.Node, error) {
	set := req.Settings
	id := uuid.New()
	args := TaskArgs{TreeID: id, NodeID: id, Role: RolePlan, Model: set.Planner.Model, Priority: req.Priority}
	if set.MaxDepth == 0 {
		args.Role, args.Model = RoleExec, set.Worker.Model
	}
	root := &store.Node{
		ID: id, TreeID: id, Depth: 0, Prompt: req.Prompt, Model: set.Worker.Model, Role: string(args.Role), Status: store.StatusQueued,
	}
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if err := settings.SnapshotTree(ctx, tx, id, req.Prompt, set); err != nil {
			return err
		}
		if err := store.InsertNodes(ctx, tx, root); err != nil {
			return err
		}
		_, err := client.InsertTx(ctx, tx, args, nil)
		return err
	})
	if err != nil {
		return nil, err
	}
	return root, nil
}

// CancelTree marks a tree's unfinished nodes cancelled and cancels their jobs.
// Running jobs get their context cancelled by River and stop streaming.
func CancelTree(ctx context.Context, db *pgxpool.Pool, client *river.Client[pgx.Tx], bus Events, treeID uuid.UUID) (int, error) {
	if _, err := store.CancelTree(ctx, db, treeID); err != nil {
		return 0, err
	}
	params := river.NewJobListParams().
		Kinds(Kind).
		States(rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRetryable,
			rivertype.JobStateRunning, rivertype.JobStateScheduled).
		Where("args ->> 'tree_id' = @tree_id", river.NamedArgs{"tree_id": treeID.String()}).
		First(10_000)
	res, err := client.JobList(ctx, params)
	if err != nil {
		return 0, fmt.Errorf("list tree jobs: %w", err)
	}
	for _, j := range res.Jobs {
		if _, err := client.JobCancel(ctx, j.ID); err != nil {
			return 0, fmt.Errorf("cancel job %d: %w", j.ID, err)
		}
	}
	if err := bus.Publish(ctx, events.Event{Type: events.TreeCancelled, TreeID: treeID, NodeID: treeID}); err != nil {
		return len(res.Jobs), err
	}
	return len(res.Jobs), nil
}
