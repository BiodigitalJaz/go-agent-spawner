// Package events carries live task-tree progress (lifecycle changes and token
// deltas) over NATS JetStream. It is deliberately separate from River: tokens
// are high-rate and disposable, so they never touch Postgres. JetStream keeps a
// short replay window so SSE clients can reconnect with Last-Event-ID.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

type Type string

const (
	TreeCreated   Type = "tree.created"
	NodeStarted   Type = "node.started"
	Delta         Type = "delta"
	Thinking      Type = "thinking" // reasoning tokens, sent so clients see progress before content
	NodeSplit     Type = "node.split"
	NodeDone      Type = "node.done"
	NodeFailed    Type = "node.failed"
	TreeDone      Type = "tree.done"
	TreeFailed    Type = "tree.failed"
	TreeCancelled Type = "tree.cancelled"
)

// Final reports whether no further events will follow for the tree.
func (t Type) Final() bool { return t == TreeDone || t == TreeFailed || t == TreeCancelled }

type Child struct {
	ID     uuid.UUID `json:"id"`
	Title  string    `json:"title"`
	Role   string    `json:"role"`
	Prompt string    `json:"prompt"`
}

type Stats struct {
	Endpoint        string  `json:"endpoint,omitempty"`
	Model           string  `json:"model,omitempty"`
	TTFTMillis      int64   `json:"ttft_ms"`
	DurationMillis  int64   `json:"duration_ms"`
	Tokens          int     `json:"tokens"`
	PromptTokens    int     `json:"prompt_tokens,omitempty"`
	TokensPerSecond float64 `json:"tokens_per_second"`
	// Merge guard: consolidation passes and batches run before the final
	// merge, and inputs truncated because they alone exceeded the context.
	MergePasses  int `json:"merge_passes,omitempty"`
	MergeBatches int `json:"merge_batches,omitempty"`
	Truncated    int `json:"truncated,omitempty"`
	// Checked: the final answer went through the rules check against the
	// tree's reference material; CheckError says why it could not.
	Checked    bool   `json:"checked,omitempty"`
	CheckError string `json:"check_error,omitempty"`
	// SourceChecked: an answer written from web search results was reviewed
	// against them (unsupported specifics removed or marked unverified).
	SourceChecked bool `json:"source_checked,omitempty"`
}

type Event struct {
	Type     Type       `json:"type"`
	TreeID   uuid.UUID  `json:"tree_id"`
	NodeID   uuid.UUID  `json:"node_id"`
	ParentID *uuid.UUID `json:"parent_id,omitempty"`
	Role     string     `json:"role,omitempty"`
	Title    string     `json:"title,omitempty"`
	Text     string     `json:"text,omitempty"`
	// TextTruncated: Text was cut to MaxEventText; the full text is in
	// Postgres (GET /v1/trees/{id}).
	TextTruncated bool      `json:"text_truncated,omitempty"`
	Children      []Child   `json:"children,omitempty"`
	Stats         *Stats    `json:"stats,omitempty"`
	Time          time.Time `json:"time"`
}

const (
	streamName    = "AGENT_EVENTS"
	subjectPrefix = "agents.tree."

	// MaxEventText caps the text one event carries. NATS refuses messages
	// over 1 MB by default; results of any size live in Postgres, so events
	// only need enough for live display.
	MaxEventText = 64 << 10
)

// capText cuts an event's text to MaxEventText on a rune boundary.
func capText(e *Event) {
	if len(e.Text) <= MaxEventText {
		return
	}
	e.Text = strings.ToValidUTF8(e.Text[:MaxEventText], "")
	e.TextTruncated = true
}

// chunks splits streamed text into pieces of at most MaxEventText bytes,
// on rune boundaries, so a large burst (a code merge's whole result) is
// sent as several deltas instead of one oversized message.
func chunks(s string) []string {
	var out []string
	for len(s) > MaxEventText {
		cut := MaxEventText
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	return append(out, s)
}

func subject(e Event) string {
	return fmt.Sprintf("%s%s.%s.%s", subjectPrefix, e.TreeID, e.NodeID, e.Type)
}

type Config struct {
	URL       string
	Retention time.Duration
	MaxBytes  int64
	Replicas  int
	FileStore bool
}

type Bus struct {
	log *slog.Logger
	nc  *nats.Conn
	js  jetstream.JetStream
}

func Connect(ctx context.Context, log *slog.Logger, cfg Config) (*Bus, error) {
	nc, err := nats.Connect(cfg.URL,
		nats.Name("agent-spawner"),
		nats.MaxReconnects(-1),
		nats.RetryOnFailedConnect(true),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				log.Warn("nats disconnected", "err", err)
			}
		}),
		nats.ReconnectHandler(func(*nats.Conn) { log.Info("nats reconnected") }),
	)
	if err != nil {
		return nil, fmt.Errorf("nats connect: %w", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, err
	}

	storage := jetstream.MemoryStorage
	if cfg.FileStore {
		storage = jetstream.FileStorage
	}
	_, err = js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      streamName,
		Subjects:  []string{subjectPrefix + ">"},
		Storage:   storage,
		MaxAge:    cmpOr(cfg.Retention, 15*time.Minute),
		MaxBytes:  cmpOr(cfg.MaxBytes, 512<<20),
		Replicas:  cmpOr(cfg.Replicas, 1),
		Discard:   jetstream.DiscardOld,
		Retention: jetstream.LimitsPolicy,
	})
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("ensure stream: %w", err)
	}
	return &Bus{log: log, nc: nc, js: js}, nil
}

func cmpOr[T comparable](v, def T) T {
	var zero T
	if v == zero {
		return def
	}
	return v
}

func (b *Bus) Close() {
	_ = b.nc.Drain()
}

// Publish sends a lifecycle event and waits for the stream ack.
func (b *Bus) Publish(ctx context.Context, e Event) error {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	capText(&e)
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = b.js.Publish(ctx, subject(e), data)
	return err
}

func (b *Bus) publishAsync(e Event) {
	e.Time = time.Now()
	data, err := json.Marshal(e)
	if err != nil {
		return
	}
	if _, err := b.js.PublishAsync(subject(e), data); err != nil {
		b.log.Warn("delta publish failed", "tree", e.TreeID, "err", err)
	}
}

// Deltas returns a writer that batches token fragments of one type (Delta or
// Thinking) for one node.
func (b *Bus) Deltas(typ Type, treeID, nodeID uuid.UUID, interval time.Duration) *DeltaWriter {
	return NewDeltaWriter(interval, func(text string) {
		b.publishAsync(Event{Type: typ, TreeID: treeID, NodeID: nodeID, Text: text})
	})
}

// DeltaWriter batches deltas into one message per interval. The first
// fragment is sent immediately so clients see time-to-first-token unbuffered.
type DeltaWriter struct {
	emit     func(string)
	mu       sync.Mutex
	buf      strings.Builder
	first    bool
	stop     chan struct{}
	stopped  chan struct{}
	stopOnce sync.Once
}

func NewDeltaWriter(interval time.Duration, emit func(string)) *DeltaWriter {
	w := &DeltaWriter{emit: emit, first: true, stop: make(chan struct{}), stopped: make(chan struct{})}
	go w.loop(cmpOr(interval, 50*time.Millisecond))
	return w
}

func (w *DeltaWriter) Write(s string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.first {
		w.first = false
		w.emit(s)
		return
	}
	w.buf.WriteString(s)
}

func (w *DeltaWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf.Len() > 0 {
		for _, c := range chunks(w.buf.String()) {
			w.emit(c)
		}
		w.buf.Reset()
	}
}

func (w *DeltaWriter) loop(interval time.Duration) {
	defer close(w.stopped)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			w.flush()
		case <-w.stop:
			w.flush()
			return
		}
	}
}

// Close flushes what is buffered. Call it before publishing the node's
// terminal event so ordering on the connection is preserved.
func (w *DeltaWriter) Close() {
	w.stopOnce.Do(func() { close(w.stop) })
	<-w.stopped
}

type Message struct {
	Seq   uint64
	Event Event
}

// Subscribe streams a tree's events, replaying from the start of the retention
// window or from just after afterSeq. The channel closes when ctx is done.
func (b *Bus) Subscribe(ctx context.Context, treeID uuid.UUID, afterSeq uint64) (<-chan Message, error) {
	cfg := jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{fmt.Sprintf("%s%s.>", subjectPrefix, treeID)},
		DeliverPolicy:  jetstream.DeliverAllPolicy,
	}
	if afterSeq > 0 {
		cfg.DeliverPolicy = jetstream.DeliverByStartSequencePolicy
		cfg.OptStartSeq = afterSeq + 1
	}
	return b.consume(ctx, cfg)
}

// SubscribeLifecycle streams node and tree lifecycle events for every tree
// (no token deltas), starting from now. It drives the global activity view.
func (b *Bus) SubscribeLifecycle(ctx context.Context) (<-chan Message, error) {
	return b.consume(ctx, jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{subjectPrefix + "*.*.node.>", subjectPrefix + "*.*.tree.>"},
		DeliverPolicy:  jetstream.DeliverNewPolicy,
	})
}

func (b *Bus) consume(ctx context.Context, cfg jetstream.OrderedConsumerConfig) (<-chan Message, error) {
	cons, err := b.js.OrderedConsumer(ctx, streamName, cfg)
	if err != nil {
		return nil, err
	}
	iter, err := cons.Messages()
	if err != nil {
		return nil, err
	}

	out := make(chan Message, 64)
	go func() {
		<-ctx.Done()
		iter.Stop()
	}()
	go func() {
		defer close(out)
		for {
			msg, err := iter.Next()
			if err != nil {
				return // iterator stopped or consumer gone
			}
			md, err := msg.Metadata()
			if err != nil {
				continue
			}
			var e Event
			if err := json.Unmarshal(msg.Data(), &e); err != nil {
				b.log.Warn("bad event payload", "subject", msg.Subject(), "err", err)
				continue
			}
			select {
			case out <- Message{Seq: md.Sequence.Stream, Event: e}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// WorkerStatus is a worker replica's heartbeat: the live limiter state of
// every backend endpoint it drives. It goes over core NATS (no persistence);
// a worker that stops sending simply ages out of the API's view.
type WorkerStatus struct {
	Worker   string          `json:"worker"`
	Queues   []string        `json:"queues"`
	Backends json.RawMessage `json:"backends"`
	Time     time.Time       `json:"time"`
}

const workerSubject = "agents.workers."

func (b *Bus) PublishWorkerStatus(ws WorkerStatus) error {
	ws.Time = time.Now()
	data, err := json.Marshal(ws)
	if err != nil {
		return err
	}
	return b.nc.Publish(workerSubject+ws.Worker, data)
}

// WatchWorkers calls fn for every worker heartbeat until ctx is done.
func (b *Bus) WatchWorkers(ctx context.Context, fn func(WorkerStatus)) error {
	sub, err := b.nc.Subscribe(workerSubject+"*", func(m *nats.Msg) {
		var ws WorkerStatus
		if err := json.Unmarshal(m.Data, &ws); err == nil {
			fn(ws)
		}
	})
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		_ = sub.Unsubscribe()
	}()
	return nil
}

const modelsChangedSubject = "agents.models.changed"

// PublishModelsChanged tells every replica to reload the model registry.
func (b *Bus) PublishModelsChanged() error {
	return b.nc.Publish(modelsChangedSubject, nil)
}

// WatchModelsChanged calls fn whenever any replica changes the model set.
func (b *Bus) WatchModelsChanged(ctx context.Context, fn func()) error {
	sub, err := b.nc.Subscribe(modelsChangedSubject, func(*nats.Msg) { fn() })
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		_ = sub.Unsubscribe()
	}()
	return nil
}

// PurgeTree removes a tree's events (token streams, progress) from the
// replay window.
func (b *Bus) PurgeTree(ctx context.Context, treeID uuid.UUID) error {
	stream, err := b.js.Stream(ctx, streamName)
	if err != nil {
		return err
	}
	return stream.Purge(ctx, jetstream.WithPurgeSubject(fmt.Sprintf("%s%s.>", subjectPrefix, treeID)))
}
