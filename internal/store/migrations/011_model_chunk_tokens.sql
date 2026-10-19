-- Safe chunk size: the largest document chunk (estimated tokens, chars/4)
-- the model reads without missing facts, measured with
-- experiments/chunking/chunk_size_sweep.py. 0 means the default rule.
ALTER TABLE agent_model ADD COLUMN chunk_tokens int NOT NULL DEFAULT 0;
