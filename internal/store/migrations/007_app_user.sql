-- Console accounts (email + password). Passwords are argon2id hashes;
-- session_epoch increments on a password change to revoke old sessions.
CREATE TABLE app_user (
    id            uuid PRIMARY KEY,
    email         text NOT NULL UNIQUE CHECK (email = lower(email)),
    password_hash text NOT NULL,
    session_epoch int NOT NULL DEFAULT 0,
    disabled      boolean NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- The key that signs account sessions, generated once and shared by every
-- API replica.
CREATE TABLE auth_secret (
    id  int PRIMARY KEY CHECK (id = 1),
    key bytea NOT NULL
);
