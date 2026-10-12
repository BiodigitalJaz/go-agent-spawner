-- Context length sizes merges; slots caps concurrent requests across all
-- worker replicas to the server's parallel capacity.
ALTER TABLE agent_model
    ADD COLUMN context_length int NOT NULL DEFAULT 0,
    ADD COLUMN slots          int NOT NULL DEFAULT 0;
