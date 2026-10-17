package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNoUser = errors.New("no such account")

type User struct {
	ID       uuid.UUID
	Email    string
	Hash     string
	Epoch    int
	Disabled bool
}

// Users is the account store (Postgres in production, a map in tests).
type Users interface {
	ByEmail(ctx context.Context, email string) (*User, error)
	ByID(ctx context.Context, id uuid.UUID) (*User, error)
	// SetPassword stores a new hash and bumps the session epoch, revoking
	// every existing session of the account. It returns the new epoch.
	SetPassword(ctx context.Context, id uuid.UUID, hash string) (int, error)
	Any(ctx context.Context) (bool, error)
	// SessionKey returns the shared key that signs account sessions.
	SessionKey(ctx context.Context) ([]byte, error)
}

func NormalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

type PGUsers struct{ DB *pgxpool.Pool }

const userCols = `id, email, password_hash, session_epoch, disabled`

func scanUser(row pgx.Row) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.Hash, &u.Epoch, &u.Disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoUser
	}
	return &u, err
}

func (p PGUsers) ByEmail(ctx context.Context, email string) (*User, error) {
	return scanUser(p.DB.QueryRow(ctx, `SELECT `+userCols+` FROM app_user WHERE email = $1`, NormalizeEmail(email)))
}

func (p PGUsers) ByID(ctx context.Context, id uuid.UUID) (*User, error) {
	return scanUser(p.DB.QueryRow(ctx, `SELECT `+userCols+` FROM app_user WHERE id = $1`, id))
}

func (p PGUsers) SetPassword(ctx context.Context, id uuid.UUID, hash string) (int, error) {
	var epoch int
	err := p.DB.QueryRow(ctx, `UPDATE app_user SET password_hash = $2, session_epoch = session_epoch + 1, updated_at = now()
		WHERE id = $1 RETURNING session_epoch`, id, hash).Scan(&epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNoUser
	}
	return epoch, err
}

func (p PGUsers) Any(ctx context.Context) (bool, error) {
	var ok bool
	err := p.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM app_user WHERE NOT disabled)`).Scan(&ok)
	return ok, err
}

func (p PGUsers) SessionKey(ctx context.Context) ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if _, err := p.DB.Exec(ctx, `INSERT INTO auth_secret (id, key) VALUES (1, $1) ON CONFLICT (id) DO NOTHING`, key); err != nil {
		return nil, err
	}
	var stored []byte
	err := p.DB.QueryRow(ctx, `SELECT key FROM auth_secret WHERE id = 1`).Scan(&stored)
	return stored, err
}

// UpsertUser creates an account or replaces its password (bumping the
// session epoch), and enables it. Used by the `spawner user` command.
func (p PGUsers) UpsertUser(ctx context.Context, email, hash string) (created bool, err error) {
	email = NormalizeEmail(email)
	tag, err := p.DB.Exec(ctx, `INSERT INTO app_user (id, email, password_hash) VALUES ($1, $2, $3)
		ON CONFLICT (email) DO NOTHING`, uuid.New(), email, hash)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 1 {
		return true, nil
	}
	_, err = p.DB.Exec(ctx, `UPDATE app_user SET password_hash = $2, session_epoch = session_epoch + 1, disabled = false, updated_at = now()
		WHERE email = $1`, email, hash)
	return false, err
}

// SetDisabled disables or enables an account; disabling also ends its sessions.
func (p PGUsers) SetDisabled(ctx context.Context, email string, disabled bool) error {
	tag, err := p.DB.Exec(ctx, `UPDATE app_user SET disabled = $2, session_epoch = session_epoch + CASE WHEN $2 THEN 1 ELSE 0 END, updated_at = now()
		WHERE email = $1`, NormalizeEmail(email), disabled)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNoUser
	}
	return err
}

type UserSummary struct {
	Email     string
	Disabled  bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (p PGUsers) List(ctx context.Context) ([]UserSummary, error) {
	rows, err := p.DB.Query(ctx, `SELECT email, disabled, created_at, updated_at FROM app_user ORDER BY email`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserSummary
	for rows.Next() {
		var u UserSummary
		if err := rows.Scan(&u.Email, &u.Disabled, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
