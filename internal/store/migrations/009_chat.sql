-- Chat: conversations of the console's conversational agent. Each assistant
-- reply is a tree (agent run); its text is the tree root's output.
CREATE TABLE chat_conversation (
    id         uuid PRIMARY KEY,
    owner      text NOT NULL DEFAULT '',  -- account email, "token:<id>", or "" without auth
    title      text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX chat_conversation_owner_idx ON chat_conversation (owner, updated_at DESC);

CREATE TABLE chat_message (
    id              uuid PRIMARY KEY,
    conversation_id uuid NOT NULL REFERENCES chat_conversation (id) ON DELETE CASCADE,
    role            text NOT NULL CHECK (role IN ('user', 'assistant')),
    content         text NOT NULL DEFAULT '',
    tree_id         uuid,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX chat_message_conversation_idx ON chat_message (conversation_id, created_at);

-- Earlier turns a chat reply's root agent sees, oldest first.
ALTER TABLE agent_tree ADD COLUMN history jsonb NOT NULL DEFAULT '[]';
