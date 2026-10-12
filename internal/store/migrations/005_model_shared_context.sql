-- The model's context is one pool shared by all concurrent requests (LM
-- Studio / llama.cpp unified KV), so admission is budgeted by tokens.
ALTER TABLE agent_model ADD COLUMN shared_context boolean NOT NULL DEFAULT false;
