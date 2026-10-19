-- Benchmarks: replays of a chat against several pool models, scored by a
-- judge model. Config and results are JSON documents owned by the API.
CREATE TABLE benchmark (
    id           uuid PRIMARY KEY,
    owner        text NOT NULL DEFAULT '',
    status       text NOT NULL,              -- running | cancelling | done | failed | cancelled
    config       jsonb NOT NULL,
    results      jsonb NOT NULL DEFAULT '{}',
    error        text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    heartbeat_at timestamptz NOT NULL DEFAULT now(), -- the runner saves progress at least every 30 s
    finished_at  timestamptz
);
CREATE INDEX benchmark_owner_idx ON benchmark (owner, created_at DESC);
