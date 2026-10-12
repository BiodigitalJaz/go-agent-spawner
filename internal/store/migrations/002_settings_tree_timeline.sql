-- Editable agent settings (role models, generation params, directives).
-- A single row, versioned for optimistic concurrency from the UI.
CREATE TABLE agent_settings (
    id         int PRIMARY KEY CHECK (id = 1),
    version    int NOT NULL,
    config     jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- One row per tree. config is the settings snapshot taken at submission, so
-- edits in the UI never change a tree that is already running.
CREATE TABLE agent_tree (
    id         uuid PRIMARY KEY,
    prompt     text NOT NULL,
    config     jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX agent_tree_created_at_idx ON agent_tree (created_at DESC);

-- Timeline columns so the UI can draw a node's phases after its live events
-- have aged out of the NATS replay window.
ALTER TABLE agent_node
    ADD COLUMN role              text NOT NULL DEFAULT '',
    ADD COLUMN started_at        timestamptz,
    ADD COLUMN split_at          timestamptz,
    ADD COLUMN reduce_started_at timestamptz,
    ADD COLUMN finished_at       timestamptz,
    ADD COLUMN stats             jsonb;
