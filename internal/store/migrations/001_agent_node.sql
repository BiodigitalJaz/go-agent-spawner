-- One row per node in an agent task tree. The root node's id is the tree id.
CREATE TABLE agent_node (
    id               uuid PRIMARY KEY,
    tree_id          uuid NOT NULL,
    parent_id        uuid REFERENCES agent_node (id) ON DELETE CASCADE,
    idx              int NOT NULL DEFAULT 0,
    depth            int NOT NULL,
    title            text NOT NULL DEFAULT '',
    prompt           text NOT NULL,
    model            text NOT NULL,
    status           text NOT NULL,
    pending_children int NOT NULL DEFAULT 0,
    output           text,
    error            text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX agent_node_tree_id_idx ON agent_node (tree_id);
CREATE INDEX agent_node_parent_id_idx ON agent_node (parent_id, idx);

-- Lets tree cancellation find unfinished jobs without scanning river_job.
CREATE INDEX river_job_agent_tree_idx ON river_job ((args ->> 'tree_id'))
    WHERE finalized_at IS NULL AND kind = 'agent_task';
