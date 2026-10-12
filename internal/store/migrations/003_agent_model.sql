-- Models added from the console. Models in backends.json stay read-only and
-- win on a name clash; these rows extend the pool at runtime.
CREATE TABLE agent_model (
    name          text PRIMARY KEY,
    model         text NOT NULL,
    endpoints     jsonb NOT NULL,
    initial_limit int NOT NULL DEFAULT 0,
    max_limit     int NOT NULL DEFAULT 0,
    extra         jsonb,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
