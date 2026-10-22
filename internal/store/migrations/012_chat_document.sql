-- Documents uploaded to a chat conversation, as extracted text. Every
-- message in the conversation can use them.
CREATE TABLE chat_document (
    id              uuid PRIMARY KEY,
    conversation_id uuid NOT NULL REFERENCES chat_conversation (id) ON DELETE CASCADE,
    name            text NOT NULL,
    kind            text NOT NULL,
    content         text NOT NULL,
    chars           int  NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX chat_document_conversation_idx ON chat_document (conversation_id, created_at);
