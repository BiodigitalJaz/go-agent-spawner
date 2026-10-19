package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/BiodigitalJaz/go-agent-spawner/internal/agent"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/auth"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/settings"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/store"
)

// Chat: a conversational agent. Each reply is a run whose root agent sees
// the earlier conversation, so chat gets streaming, model choice, link
// fetching, web search and (on request) the agent pool. Conversations
// belong to the account (or token) that created them.

const (
	maxChatMessage      = 32_000 // characters in one message
	maxHistoryReplyChar = 6_000  // a long earlier reply is shortened in the history
)

func (s *Server) chatRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/chats", s.listChats)
	mux.HandleFunc("POST /v1/chats", s.createChat)
	mux.HandleFunc("GET /v1/chats/{id}", s.getChat)
	mux.HandleFunc("PATCH /v1/chats/{id}", s.renameChat)
	mux.HandleFunc("DELETE /v1/chats/{id}", s.deleteChat)
	mux.HandleFunc("POST /v1/chats/{id}/messages", s.sendChatMessage)
}

func chatID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid chat id")
		return uuid.Nil, false
	}
	return id, true
}

func (s *Server) listChats(w http.ResponseWriter, r *http.Request) {
	list, err := store.ListConversations(r.Context(), s.db, auth.Principal(r.Context()), 200)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"chats": list})
}

func (s *Server) createChat(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title string `json:"title"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body)
	c, err := store.CreateConversation(r.Context(), s.db, auth.Principal(r.Context()), titleFrom(body.Title))
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) getChat(w http.ResponseWriter, r *http.Request) {
	id, ok := chatID(w, r)
	if !ok {
		return
	}
	c, err := store.GetConversation(r.Context(), s.db, id, auth.Principal(r.Context()))
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "chat not found")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	msgs, err := store.ChatMessages(r.Context(), s.db, id)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"chat": c, "messages": msgs})
}

func (s *Server) renameChat(w http.ResponseWriter, r *http.Request) {
	id, ok := chatID(w, r)
	if !ok {
		return
	}
	var body struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil || strings.TrimSpace(body.Title) == "" {
		httpError(w, http.StatusBadRequest, "title is required")
		return
	}
	if err := store.RenameConversation(r.Context(), s.db, id, auth.Principal(r.Context()), titleFrom(body.Title)); errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "chat not found")
		return
	} else if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "title": titleFrom(body.Title)})
}

// deleteChat removes the conversation and purges the runs of its replies
// (cancelling any still running first).
func (s *Server) deleteChat(w http.ResponseWriter, r *http.Request) {
	id, ok := chatID(w, r)
	if !ok {
		return
	}
	trees, err := store.DeleteConversation(r.Context(), s.db, id, auth.Principal(r.Context()))
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "chat not found")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	purged := 0
	for _, t := range trees {
		if s.purgeRun(r.Context(), t) == nil {
			purged++
		}
	}
	s.log.Info("chat deleted", "chat", id, "runs", len(trees), "purged", purged, "remote", r.RemoteAddr)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true, "runs_purged": purged, "runs": len(trees)})
}

// purgeRun cancels a run if needed, waits briefly for its agents to stop,
// and deletes every record of it.
func (s *Server) purgeRun(ctx context.Context, id uuid.UUID) error {
	root, err := store.GetNode(ctx, s.db, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil // already gone
	}
	if err != nil {
		return err
	}
	if !root.Status.Terminal() {
		if _, err := agent.CancelTree(ctx, s.db, s.river, s.bus, id); err != nil {
			return err
		}
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
			_, _, err := store.PurgeTree(ctx, tx, id)
			return err
		})
		if !errors.Is(err, store.ErrTreeBusy) || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		s.log.Warn("purge chat run", "tree", id, "err", err)
		return err
	}
	_ = s.bus.PurgeTree(ctx, id)
	return nil
}

type chatMessageRequest struct {
	Content string `json:"content"`
	Model   string `json:"model,omitempty"`
	Search  bool   `json:"search,omitempty"` // older clients: same as search_mode "on"
	// SearchMode: "auto" (default; search when the question needs current
	// or local facts), "on" or "off".
	SearchMode string `json:"search_mode,omitempty"`
	FetchURLs  *bool  `json:"fetch_urls,omitempty"` // default true
	// Deep lets the planner split the reply across the agent pool; by
	// default one agent answers, which is fastest for conversation.
	Deep bool `json:"deep,omitempty"`
}

func (s *Server) sendChatMessage(w http.ResponseWriter, r *http.Request) {
	id, ok := chatID(w, r)
	if !ok {
		return
	}
	var body chatMessageRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	content := strings.TrimSpace(body.Content)
	if content == "" || len(content) > maxChatMessage {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("a message needs 1-%d characters", maxChatMessage))
		return
	}
	owner := auth.Principal(r.Context())
	c, err := store.GetConversation(r.Context(), s.db, id, owner)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "chat not found")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	msgs, err := store.ChatMessages(r.Context(), s.db, id)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n := len(msgs); n > 0 && msgs[n-1].Role == "assistant" && !store.Status(msgs[n-1].Status).Terminal() && msgs[n-1].Status != "" {
		httpError(w, http.StatusConflict, "the previous reply is still being written; wait for it or stop it")
		return
	}

	req := createRequest{Prompt: content, Model: body.Model, FetchURLs: body.FetchURLs == nil || *body.FetchURLs, softFetch: true}
	switch {
	case body.Search || body.SearchMode == "on":
		req.Search = s.SearchEnabled // forced; never an error in chat
	case body.SearchMode == "" || body.SearchMode == "auto":
		req.SearchAuto = true
	case body.SearchMode == "off":
	default:
		httpError(w, http.StatusBadRequest, `search_mode must be "auto", "on" or "off"`)
		return
	}
	if !body.Deep {
		zero := 0
		req.MaxDepth = &zero
	}
	model := body.Model
	if model == "" {
		if st, err := settings.Load(r.Context(), s.db, s.defaults); err == nil {
			model = st.Settings.Worker.Model
		}
	}
	history, dropped := chatHistory(msgs, agent.MaxReferenceChars(s.contextLength(model))*3/4)
	resp, treeID, err := s.startRun(r.Context(), req, history, r.RemoteAddr)
	if err != nil {
		s.writeRunError(w, err)
		return
	}
	userID, assistantID, err := store.AddExchange(r.Context(), s.db, id, content, treeID, titleFrom(content))
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp["chat_id"] = c.ID
	resp["user_message_id"] = userID
	resp["assistant_message_id"] = assistantID
	if dropped > 0 {
		resp["history_note"] = fmt.Sprintf("the %d oldest messages were left out to fit the model's context", dropped)
	}
	writeJSON(w, http.StatusCreated, resp)
}

// chatHistory turns stored messages into the turns a reply's agent sees,
// newest kept first within budget characters. Exchanges whose reply failed
// are left out; long replies are shortened. It returns how many messages
// were dropped for space.
func chatHistory(msgs []store.ChatMessage, budget int) ([]settings.Turn, int) {
	var turns []settings.Turn
	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		if m.Role != "user" {
			continue
		}
		var reply *store.ChatMessage
		if i+1 < len(msgs) && msgs[i+1].Role == "assistant" {
			reply = &msgs[i+1]
		}
		if reply == nil || reply.Status != string(store.StatusDone) || strings.TrimSpace(reply.Content) == "" {
			continue
		}
		turns = append(turns, settings.Turn{Role: "user", Content: m.Content}, settings.Turn{Role: "assistant", Content: shorten(reply.Content, maxHistoryReplyChar)})
	}
	used, keep := 0, len(turns)
	for keep > 0 && used+len(turns[keep-1].Content)+len(turns[keep-2].Content) <= budget {
		used += len(turns[keep-1].Content) + len(turns[keep-2].Content)
		keep -= 2
	}
	return turns[keep:], keep
}

func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	head := strings.ToValidUTF8(s[:n*2/3], "")
	tail := strings.ToValidUTF8(s[len(s)-n/3:], "")
	return head + "\n[… part of this earlier reply was left out …]\n" + tail
}

func titleFrom(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 60 {
		s = strings.ToValidUTF8(s[:60], "") + "…"
	}
	return s
}
