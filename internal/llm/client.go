// Package llm is a minimal client for OpenAI-compatible chat completion APIs
// (LM Studio, vLLM, llama.cpp server). It only uses the portable subset of the
// API so backends can be swapped without code changes.
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Request struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature *float64  `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	// JSONSchema, when set, constrains output via response_format json_schema.
	JSONSchema *Schema `json:"-"`
	// Extra fields merged into the request body for backend-specific knobs,
	// e.g. {"reasoning_effort": "low"} or vLLM's chat_template_kwargs.
	Extra map[string]any `json:"-"`
}

type DeltaKind int

const (
	Content DeltaKind = iota
	Reasoning
)

type Schema struct {
	Name   string
	Schema json.RawMessage
}

type Stats struct {
	TTFT             time.Duration
	Duration         time.Duration
	PromptTokens     int
	CompletionTokens int
}

func (s Stats) TokensPerSecond() float64 {
	gen := s.Duration - s.TTFT
	if gen <= 0 || s.CompletionTokens == 0 {
		return 0
	}
	return float64(s.CompletionTokens) / gen.Seconds()
}

// StatusError is a non-2xx response from the backend.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("llm: backend returned %d: %s", e.Code, e.Body)
}

// Overloaded reports whether err means the backend is congested or
// unreachable, as opposed to the request itself being bad.
func Overloaded(err error) bool {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code == http.StatusTooManyRequests || se.Code >= 500
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	// Transport errors: connection refused, reset, header timeout.
	return err != nil
}

// Retryable reports whether retrying the same request could succeed. Only 4xx
// responses (bad request, unknown model, context too long) are permanent;
// transport errors and malformed model output are worth another attempt.
func Retryable(err error) bool {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code == http.StatusTooManyRequests || se.Code == http.StatusRequestTimeout || se.Code >= 500
	}
	return true
}

// ErrTruncated means the stream ended without [DONE] or a finish_reason: the
// backend dropped the connection or crashed mid-generation. The partial
// output must not be treated as a complete answer.
var ErrTruncated = errors.New("llm: stream ended before completion")

// DecodeError means the response was received but could not be parsed.
type DecodeError struct{ Err error }

func (e *DecodeError) Error() string { return "llm: decode response: " + e.Err.Error() }
func (e *DecodeError) Unwrap() error { return e.Err }

type Client struct {
	BaseURL string // e.g. http://10.0.0.5:1234 (the /v1 prefix is added)
	APIKey  string
	HTTP    *http.Client
}

func New(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		HTTP: &http.Client{Transport: &http.Transport{
			MaxIdleConnsPerHost:   64,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: 2 * time.Minute, // prefill of long prompts can be slow
		}},
	}
}

type wireRequest struct {
	Request
	Stream         bool            `json:"stream"`
	StreamOptions  *streamOptions  `json:"stream_options,omitempty"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type responseFormat struct {
	Type       string     `json:"type"`
	JSONSchema jsonSchema `json:"json_schema"`
}

type jsonSchema struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

type chunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	// Some servers report failures inside an already-started stream.
	Error *struct {
		Message string `json:"message"`
		Code    any    `json:"code"`
	} `json:"error"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// Stream runs a streaming completion, calling onDelta for each content and
// reasoning fragment, and returns the full content. TTFT is measured to the
// first fragment of either kind, since that is when the backend started
// generating.
func (c *Client) Stream(ctx context.Context, req Request, onDelta func(DeltaKind, string)) (string, Stats, error) {
	wire := wireRequest{Request: req, Stream: true, StreamOptions: &streamOptions{IncludeUsage: true}}
	if req.JSONSchema != nil {
		wire.ResponseFormat = &responseFormat{
			Type:       "json_schema",
			JSONSchema: jsonSchema{Name: req.JSONSchema.Name, Strict: true, Schema: req.JSONSchema.Schema},
		}
	}
	body, err := marshalWithExtra(wire, req.Extra)
	if err != nil {
		return "", Stats{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", Stats{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	var stats Stats
	start := time.Now()
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return "", stats, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", stats, &StatusError{Code: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}

	var out strings.Builder
	chunks := 0
	finished := false
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		data, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			continue
		}
		data = bytes.TrimSpace(data)
		if bytes.Equal(data, []byte("[DONE]")) {
			finished = true
			break
		}
		var ch chunk
		if err := json.Unmarshal(data, &ch); err != nil {
			return out.String(), stats, &DecodeError{Err: err}
		}
		if ch.Error != nil {
			return out.String(), stats, &StatusError{Code: streamErrorCode(ch.Error.Code, ch.Error.Message), Body: ch.Error.Message}
		}
		if ch.Usage != nil {
			stats.PromptTokens = ch.Usage.PromptTokens
			stats.CompletionTokens = ch.Usage.CompletionTokens
		}
		for _, choice := range ch.Choices {
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finished = true
			}
			if (choice.Delta.Content != "" || choice.Delta.ReasoningContent != "") && stats.TTFT == 0 {
				stats.TTFT = time.Since(start)
			}
			if choice.Delta.ReasoningContent != "" && onDelta != nil {
				onDelta(Reasoning, choice.Delta.ReasoningContent)
			}
			if choice.Delta.Content != "" {
				chunks++
				out.WriteString(choice.Delta.Content)
				if onDelta != nil {
					onDelta(Content, choice.Delta.Content)
				}
			}
		}
	}
	stats.Duration = time.Since(start)
	if stats.CompletionTokens == 0 {
		stats.CompletionTokens = chunks // backend did not report usage; approximate
	}
	if err := sc.Err(); err != nil {
		return out.String(), stats, err
	}
	if !finished {
		return out.String(), stats, ErrTruncated
	}
	return out.String(), stats, nil
}

// JSON runs a schema-constrained completion and decodes the result into v.
// onReasoning, if set, receives reasoning fragments; content is not forwarded
// since partial JSON is not useful to show.
func (c *Client) JSON(ctx context.Context, req Request, v any, onReasoning func(string)) (Stats, error) {
	content, stats, err := c.Stream(ctx, req, func(kind DeltaKind, text string) {
		if kind == Reasoning && onReasoning != nil {
			onReasoning(text)
		}
	})
	if err != nil {
		return stats, err
	}
	if err := json.Unmarshal([]byte(stripFences(content)), v); err != nil {
		return stats, &DecodeError{Err: err}
	}
	return stats, nil
}

var (
	embeddedStatus = regexp.MustCompile(`returned (\d{3})`)
	embeddedCode   = regexp.MustCompile(`"code"\s*:\s*(\d{3})`)
)

// streamErrorCode recovers the real status of an error reported inside a
// stream (the HTTP status was already 200), in order of reliability: an
// explicit code field, a "returned NNN" from the engine, a nested JSON
// "code", then the message text. LM Studio uses 400 for a request larger
// than the whole context (permanent) and 500 for "Context size has been
// exceeded" when concurrent requests fill a shared context (transient), so
// the code must win over keyword matching.
func streamErrorCode(code any, msg string) int {
	if f, ok := code.(float64); ok && f >= 400 && f < 600 {
		return int(f)
	}
	for _, re := range []*regexp.Regexp{embeddedStatus, embeddedCode} {
		if m := re.FindStringSubmatch(msg); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n >= 400 && n < 600 {
				return n
			}
		}
	}
	if strings.Contains(strings.ToLower(msg), "exceeds the available context") {
		return http.StatusBadRequest
	}
	return http.StatusBadGateway
}

// ContextPressure reports whether err means a shared context was full at the
// moment (other requests were using it). It is transient: retrying shortly,
// without counting a failed attempt, is the right response.
func ContextPressure(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code >= 500 && strings.Contains(strings.ToLower(se.Body), "context size has been exceeded")
}

func marshalWithExtra(wire wireRequest, extra map[string]any) ([]byte, error) {
	body, err := json.Marshal(wire)
	if err != nil || len(extra) == 0 {
		return body, err
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	for k, v := range extra {
		if _, reserved := m[k]; !reserved {
			m[k] = v
		}
	}
	return json.Marshal(m)
}

// stripFences removes a ```json fence some models add even under a schema.
func stripFences(s string) string {
	s = strings.TrimSpace(s)
	if rest, ok := strings.CutPrefix(s, "```"); ok {
		rest = strings.TrimPrefix(rest, "json")
		s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest), "```"))
	}
	return s
}
