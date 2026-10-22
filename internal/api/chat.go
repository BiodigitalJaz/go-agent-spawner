package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/BiodigitalJaz/go-agent-spawner/internal/agent"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/auth"
	"github.com/BiodigitalJaz/go-agent-spawner/internal/extract"
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
	mux.HandleFunc("POST /v1/chats/{id}/retry", s.retryChatMessage)
	mux.HandleFunc("POST /v1/chats/{id}/documents", s.uploadChatDocuments)
	mux.HandleFunc("DELETE /v1/chats/{id}/documents/{doc}", s.deleteChatDocument)
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
	docs, err := store.ChatDocuments(r.Context(), s.db, id, false)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"chat": c, "messages": msgs, "documents": docs})
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
	// SourceCheck reviews a reply written from search results against them
	// (off by default: it adds a second pass and replaces the answer).
	SourceCheck bool `json:"source_check,omitempty"`
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

	resp, treeID, ok := s.startChatRun(w, r, id, body, content, msgs)
	if !ok {
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
	writeJSON(w, http.StatusCreated, resp)
}

// startChatRun starts the run that writes a reply to content, with the
// conversation in msgs as its history. It has written the error response
// when ok is false.
func (s *Server) startChatRun(w http.ResponseWriter, r *http.Request, chatID uuid.UUID, body chatMessageRequest, content string, msgs []store.ChatMessage) (map[string]any, uuid.UUID, bool) {
	req := createRequest{Prompt: content, Model: body.Model, FetchURLs: body.FetchURLs == nil || *body.FetchURLs, softFetch: true, SourceCheck: body.SourceCheck}
	switch {
	case body.Search || body.SearchMode == "on":
		req.Search = s.SearchEnabled // forced; never an error in chat
	case body.SearchMode == "" || body.SearchMode == "auto":
		req.SearchAuto = true
	case body.SearchMode == "off":
	default:
		httpError(w, http.StatusBadRequest, `search_mode must be "auto", "on" or "off"`)
		return nil, uuid.Nil, false
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
	docs, err := store.ChatDocuments(r.Context(), s.db, chatID, true)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return nil, uuid.Nil, false
	}
	docMode := s.useDocuments(&req, docs, model, history)
	resp, treeID, err := s.startRun(r.Context(), req, history, r.RemoteAddr)
	if err != nil {
		s.writeRunError(w, err)
		return nil, uuid.Nil, false
	}
	if dropped > 0 {
		resp["history_note"] = fmt.Sprintf("the %d oldest messages were left out to fit the model's context", dropped)
	}
	if docMode != "" {
		resp["documents"] = map[string]any{"count": len(docs), "mode": docMode}
	}
	return resp, treeID, true
}

// Limits for chat documents.
const (
	maxChatDocuments = 10
	maxDocumentBytes = 20 << 20  // one uploaded file
	maxUploadBytes   = 64 << 20  // one upload request
	maxDocumentChars = 1_000_000 // extracted text of one document (~250k tokens)
	maxChatDocChars  = 2_000_000 // all of a chat's documents
	inlineDocChars   = 24_000    // read whole when no safe chunk size is known
	partNoteText     = "This item is part of the documents the user attached to the conversation. Answer the user's message above from this item only, naming the document and quoting or citing the passages you use. If this item has nothing relevant to the message, reply only: Nothing relevant in this part."
)

// useDocuments makes a chat run use the conversation's documents. Small
// ones (within the worker model's safe chunk size, and half the reference
// budget) go into the reference with the history, so every agent reads
// them whole. Larger ones become parts split at the safe chunk size, one
// agent each, merged by the model: the lossless path (experiments/chunking
// v3). It returns the mode used, or "".
func (s *Server) useDocuments(req *createRequest, docs []store.ChatDocument, model string, history []settings.Turn) string {
	if len(docs) == 0 {
		return ""
	}
	chunkTokens := 0
	if m, ok := s.registry.Get(model); ok {
		chunkTokens = m.ChunkTokens
	}
	inline := inlineDocChars
	if chunkTokens > 0 {
		inline = chunkTokens * 4
	}
	inline = min(inline, agent.MaxReferenceChars(s.contextLength(model))/2)
	total := 0
	for _, d := range docs {
		total += len(d.Text)
	}
	if total <= inline {
		req.documents = docs
		return "whole"
	}
	for _, d := range docs {
		req.Parts = append(req.Parts, agent.Part{Title: d.Name, Text: d.Text})
	}
	if chunkTokens > 0 {
		req.MaxPartTokens = chunkTokens * 4 / 3 // parts are sized at chars/3
	}
	req.Merge = "llm"
	req.Search, req.SearchAuto = false, false // the question is about the documents
	req.partNote = partNoteText
	if recent := recentTurns(history, 2, 600); recent != "" {
		req.partNote = "Earlier in the conversation (for context):\n" + recent + "\n" + partNoteText
	}
	return "parts"
}

// recentTurns renders the last n exchanges, each message shortened to max
// characters, for agents that read one part of a document.
func recentTurns(history []settings.Turn, n, max int) string {
	if len(history) > 2*n {
		history = history[len(history)-2*n:]
	}
	var b strings.Builder
	for _, t := range history {
		who := "User"
		if t.Role == "assistant" {
			who = "Assistant"
		}
		text := t.Content
		if len(text) > max {
			text = strings.ToValidUTF8(text[:max], "") + "…"
		}
		fmt.Fprintf(&b, "%s: %s\n", who, text)
	}
	return b.String()
}

// uploadChatDocuments extracts text from uploaded files (multipart field
// "files", one or more) and stores it with the conversation. Files that
// cannot be read are reported one by one; the others are kept.
func (s *Server) uploadChatDocuments(w http.ResponseWriter, r *http.Request) {
	id, ok := chatID(w, r)
	if !ok {
		return
	}
	if _, err := store.GetConversation(r.Context(), s.db, id, auth.Principal(r.Context())); errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "chat not found")
		return
	} else if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	existing, err := store.ChatDocuments(r.Context(), s.db, id, false)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	count, chars := len(existing), 0
	for _, d := range existing {
		chars += d.Chars
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	reader, err := r.MultipartReader()
	if err != nil {
		httpError(w, http.StatusBadRequest, "send the files as multipart/form-data in the field \"files\"")
		return
	}
	type failure struct {
		Name  string `json:"name"`
		Error string `json:"error"`
	}
	added, failed := []store.ChatDocument{}, []failure{}
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			httpError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the upload is too large or broken (at most %d MB per upload): %v", maxUploadBytes>>20, err))
			return
		}
		if part.FormName() != "files" || part.FileName() == "" {
			part.Close()
			continue
		}
		name := documentName(part.FileName())
		data, err := io.ReadAll(io.LimitReader(part, maxDocumentBytes+1))
		part.Close()
		switch {
		case err != nil:
			failed = append(failed, failure{name, "could not read the upload: " + err.Error()})
			continue
		case len(data) > maxDocumentBytes:
			failed = append(failed, failure{name, fmt.Sprintf("larger than %d MB", maxDocumentBytes>>20)})
			continue
		case count >= maxChatDocuments:
			failed = append(failed, failure{name, fmt.Sprintf("a chat holds at most %d documents; remove one first", maxChatDocuments)})
			continue
		}
		kind, text, err := extract.Text(name, data)
		if err != nil {
			failed = append(failed, failure{name, err.Error()})
			continue
		}
		if len(text) > maxDocumentChars {
			failed = append(failed, failure{name, fmt.Sprintf("too long: %d characters of text, the limit is %d (about %dk tokens)", len(text), maxDocumentChars, maxDocumentChars/4000)})
			continue
		}
		if chars+len(text) > maxChatDocChars {
			failed = append(failed, failure{name, fmt.Sprintf("the chat's documents would exceed %d characters; remove one first", maxChatDocChars)})
			continue
		}
		d, err := store.AddChatDocument(r.Context(), s.db, id, name, kind, text)
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		count, chars = count+1, chars+len(text)
		added = append(added, *d)
		s.log.Info("chat document added", "chat", id, "name", name, "kind", kind, "bytes", len(data), "chars", len(text))
	}
	if len(added) == 0 && len(failed) == 0 {
		httpError(w, http.StatusBadRequest, "no files in the upload (field \"files\")")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"documents": added, "errors": failed})
}

// documentName keeps a file name's last element, trimmed to a sane length.
func documentName(n string) string {
	n = strings.TrimSpace(path.Base(strings.ReplaceAll(n, "\\", "/")))
	if n == "" || n == "." || n == "/" {
		n = "document"
	}
	if len(n) > 200 {
		n = strings.ToValidUTF8(n[:200], "")
	}
	return n
}

func (s *Server) deleteChatDocument(w http.ResponseWriter, r *http.Request) {
	id, ok := chatID(w, r)
	if !ok {
		return
	}
	doc, err := uuid.Parse(r.PathValue("doc"))
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid document id")
		return
	}
	if _, err := store.GetConversation(r.Context(), s.db, id, auth.Principal(r.Context())); errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "chat not found")
		return
	} else if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := store.DeleteChatDocument(r.Context(), s.db, id, doc); errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "document not found")
		return
	} else if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": doc, "deleted": true})
}

// retryChatMessage writes the last reply again, typically with search on
// ("Search and answer again"): the reply's message is pointed at a new run
// and the old run is purged. The body is a message request without content.
func (s *Server) retryChatMessage(w http.ResponseWriter, r *http.Request) {
	id, ok := chatID(w, r)
	if !ok {
		return
	}
	var body chatMessageRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if _, err := store.GetConversation(r.Context(), s.db, id, auth.Principal(r.Context())); errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "chat not found")
		return
	} else if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	msgs, err := store.ChatMessages(r.Context(), s.db, id)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	n := len(msgs)
	if n < 2 || msgs[n-1].Role != "assistant" || msgs[n-2].Role != "user" {
		httpError(w, http.StatusBadRequest, "there is no reply to write again")
		return
	}
	if last := msgs[n-1]; last.Status != "" && !store.Status(last.Status).Terminal() {
		httpError(w, http.StatusConflict, "the reply is still being written; wait for it or stop it")
		return
	}
	resp, treeID, ok := s.startChatRun(w, r, id, body, msgs[n-2].Content, msgs[:n-2])
	if !ok {
		return
	}
	old, err := store.ReplaceReply(r.Context(), s.db, id, msgs[n-1].ID, treeID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if old != nil && *old != treeID {
		go func() { _ = s.purgeRun(context.WithoutCancel(r.Context()), *old) }()
	}
	resp["chat_id"] = id
	resp["user_message_id"] = msgs[n-2].ID
	resp["assistant_message_id"] = msgs[n-1].ID
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
