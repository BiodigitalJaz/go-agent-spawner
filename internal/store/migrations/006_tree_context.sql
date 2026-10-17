-- Reference material shared by every agent in a tree (pasted text, fetched
-- pages, rules to respect). Stored once per tree, not in each node's prompt.
ALTER TABLE agent_tree ADD COLUMN context text NOT NULL DEFAULT '';
