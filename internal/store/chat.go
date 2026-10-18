package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type Conversation struct {
	ID        uuid.UUID `json:"id"`
	Owner     string    `json:"-"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Messages  int       `json:"messages,omitempty"`
}

// ChatMessage is a stored message. An assistant message's text is its
// tree's root output, so Content is filled from the tree (Status tells
// whether it is still being written).
type ChatMessage struct {
	ID        uuid.UUID  `json:"id"`
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	TreeID    *uuid.UUID `json:"tree_id,omitempty"`
	Status    string     `json:"status,omitempty"` // the reply's tree status
	Error     string     `json:"error,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

func CreateConversation(ctx context.Context, db DBTX, owner, title string) (*Conversation, error) {
	c := &Conversation{ID: uuid.New(), Owner: owner, Title: title}
	err := db.QueryRow(ctx, `INSERT INTO chat_conversation (id, owner, title) VALUES ($1, $2, $3) RETURNING created_at, updated_at`,
		c.ID, owner, title).Scan(&c.CreatedAt, &c.UpdatedAt)
	return c, err
}

func ListConversations(ctx context.Context, db DBTX, owner string, limit int) ([]Conversation, error) {
	rows, err := db.Query(ctx, `SELECT c.id, c.title, c.created_at, c.updated_at, count(m.id)
		FROM chat_conversation c LEFT JOIN chat_message m ON m.conversation_id = c.id
		WHERE c.owner = $1 GROUP BY c.id ORDER BY c.updated_at DESC LIMIT $2`, owner, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Conversation{}
	for rows.Next() {
		var c Conversation
		if err := rows.Scan(&c.ID, &c.Title, &c.CreatedAt, &c.UpdatedAt, &c.Messages); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetConversation returns the owner's conversation, or ErrNotFound (also for
// another owner's, so ids reveal nothing).
func GetConversation(ctx context.Context, db DBTX, id uuid.UUID, owner string) (*Conversation, error) {
	c := Conversation{ID: id}
	err := db.QueryRow(ctx, `SELECT owner, title, created_at, updated_at FROM chat_conversation WHERE id = $1 AND owner = $2`, id, owner).
		Scan(&c.Owner, &c.Title, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}

func ChatMessages(ctx context.Context, db DBTX, conversationID uuid.UUID) ([]ChatMessage, error) {
	rows, err := db.Query(ctx, `SELECT m.id, m.role, m.content, m.tree_id, coalesce(n.status, ''), coalesce(n.output, ''), coalesce(n.error, ''), m.created_at
		FROM chat_message m LEFT JOIN agent_node n ON n.id = m.tree_id
		WHERE m.conversation_id = $1 ORDER BY m.created_at, m.role DESC`, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChatMessage{}
	for rows.Next() {
		var m ChatMessage
		var output string
		if err := rows.Scan(&m.ID, &m.Role, &m.Content, &m.TreeID, &m.Status, &output, &m.Error, &m.CreatedAt); err != nil {
			return nil, err
		}
		if m.Role == "assistant" && m.Content == "" {
			m.Content = output
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AddExchange stores a user message and the assistant reply being written
// by treeID, and touches the conversation (titling it from the first
// message).
func AddExchange(ctx context.Context, db DBTX, conversationID uuid.UUID, user string, treeID uuid.UUID, title string) (userID, assistantID uuid.UUID, err error) {
	userID, assistantID = uuid.New(), uuid.New()
	now := time.Now()
	if _, err = db.Exec(ctx, `INSERT INTO chat_message (id, conversation_id, role, content, created_at) VALUES ($1, $2, 'user', $3, $4)`,
		userID, conversationID, user, now); err != nil {
		return
	}
	if _, err = db.Exec(ctx, `INSERT INTO chat_message (id, conversation_id, role, tree_id, created_at) VALUES ($1, $2, 'assistant', $3, $4)`,
		assistantID, conversationID, treeID, now.Add(time.Millisecond)); err != nil {
		return
	}
	_, err = db.Exec(ctx, `UPDATE chat_conversation SET updated_at = now(), title = CASE WHEN title = '' THEN $2 ELSE title END WHERE id = $1`,
		conversationID, title)
	return
}

func RenameConversation(ctx context.Context, db DBTX, id uuid.UUID, owner, title string) error {
	tag, err := db.Exec(ctx, `UPDATE chat_conversation SET title = $3, updated_at = now() WHERE id = $1 AND owner = $2`, id, owner, title)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// DeleteConversation removes a conversation and its messages and returns
// the trees of its replies (for the caller to purge).
func DeleteConversation(ctx context.Context, db DBTX, id uuid.UUID, owner string) ([]uuid.UUID, error) {
	rows, err := db.Query(ctx, `SELECT tree_id FROM chat_message WHERE conversation_id = $1 AND tree_id IS NOT NULL
		AND EXISTS (SELECT 1 FROM chat_conversation WHERE id = $1 AND owner = $2)`, id, owner)
	if err != nil {
		return nil, err
	}
	var trees []uuid.UUID
	for rows.Next() {
		var t uuid.UUID
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return nil, err
		}
		trees = append(trees, t)
	}
	rows.Close()
	tag, err := db.Exec(ctx, `DELETE FROM chat_conversation WHERE id = $1 AND owner = $2`, id, owner)
	if err == nil && tag.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	return trees, err
}
