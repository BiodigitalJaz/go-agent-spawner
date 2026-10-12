package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func sseServer(t *testing.T, status int, frames ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if body["stream"] != true {
			t.Errorf("expected stream=true")
		}
		if body["model"] != "m" {
			t.Errorf("extra must not override reserved fields, model=%v", body["model"])
		}
		if r.Header.Get("X-Test-Extra") == "1" && body["reasoning_effort"] != "low" {
			t.Errorf("extra field missing: %v", body)
		}
		w.WriteHeader(status)
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
			w.(http.Flusher).Flush()
		}
	}))
}

func TestStream(t *testing.T) {
	srv := sseServer(t, 200,
		`{"choices":[{"delta":{"reasoning_content":"thinking"}}]}`,
		`{"choices":[{"delta":{"content":"Hel"}}]}`,
		`{"choices":[{"delta":{"content":"lo"}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":2}}`,
		`[DONE]`,
	)
	defer srv.Close()

	var deltas, reasoning []string
	out, stats, err := New(srv.URL).Stream(context.Background(), Request{Model: "m"}, func(k DeltaKind, d string) {
		if k == Reasoning {
			reasoning = append(reasoning, d)
		} else {
			deltas = append(deltas, d)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != "Hello" || strings.Join(deltas, "|") != "Hel|lo" || strings.Join(reasoning, "|") != "thinking" {
		t.Fatalf("got %q deltas %v reasoning %v", out, deltas, reasoning)
	}
	if stats.TTFT <= 0 || stats.PromptTokens != 7 || stats.CompletionTokens != 2 {
		t.Fatalf("bad stats %+v", stats)
	}
}

func TestStatusErrorClassification(t *testing.T) {
	srv := sseServer(t, 503)
	defer srv.Close()
	_, _, err := New(srv.URL).Stream(context.Background(), Request{Model: "m"}, nil)
	if !Overloaded(err) || !Retryable(err) {
		t.Fatalf("503 should be overloaded+retryable: %v", err)
	}

	bad := sseServer(t, 400)
	defer bad.Close()
	_, _, err = New(bad.URL).Stream(context.Background(), Request{Model: "m"}, nil)
	if Overloaded(err) || Retryable(err) {
		t.Fatalf("400 should be neither: %v", err)
	}
}

func TestJSONStripsFences(t *testing.T) {
	srv := sseServer(t, 200,
		`{"choices":[{"delta":{"content":"`+"```json\\n"+`{\"a\":1}`+"\\n```"+`"}}]}`,
		`[DONE]`,
	)
	defer srv.Close()
	var v struct{ A int }
	if _, err := New(srv.URL).JSON(context.Background(), Request{Model: "m", JSONSchema: &Schema{Name: "x", Schema: json.RawMessage(`{}`)}}, &v, nil); err != nil {
		t.Fatal(err)
	}
	if v.A != 1 {
		t.Fatalf("got %+v", v)
	}
}

func TestExtraMergedButNotOverriding(t *testing.T) {
	srv := sseServer(t, 200, `[DONE]`)
	defer srv.Close()
	c := New(srv.URL)
	c.HTTP.Transport = headerRT{http.DefaultTransport}
	_, _, err := c.Stream(context.Background(), Request{Model: "m", Extra: map[string]any{"reasoning_effort": "low", "model": "evil"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
}

type headerRT struct{ next http.RoundTripper }

func (h headerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Set("X-Test-Extra", "1")
	return h.next.RoundTrip(r)
}

func TestTruncatedStreamIsAnError(t *testing.T) {
	srv := sseServer(t, 200, `{"choices":[{"delta":{"content":"partial"}}]}`) // no [DONE]
	defer srv.Close()
	out, _, err := New(srv.URL).Stream(context.Background(), Request{Model: "m"}, nil)
	if !errors.Is(err, ErrTruncated) || out != "partial" || !Retryable(err) {
		t.Fatalf("want retryable ErrTruncated with partial output, got %q %v", out, err)
	}

	fin := sseServer(t, 200, `{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`) // finish_reason, no [DONE]
	defer fin.Close()
	if _, _, err := New(fin.URL).Stream(context.Background(), Request{Model: "m"}, nil); err != nil {
		t.Fatalf("finish_reason should count as complete: %v", err)
	}
}

func TestErrorPayloadMidStream(t *testing.T) {
	srv := sseServer(t, 200, `{"choices":[{"delta":{"content":"a"}}]}`, `{"error":{"message":"model crashed"}}`)
	defer srv.Close()
	_, _, err := New(srv.URL).Stream(context.Background(), Request{Model: "m"}, nil)
	var se *StatusError
	if !errors.As(err, &se) || !strings.Contains(se.Body, "model crashed") || !Retryable(err) {
		t.Fatalf("want retryable StatusError, got %v", err)
	}
}

func TestStreamErrorKeepsClientErrorsNonRetryable(t *testing.T) {
	overflow := `{"error":{"message":"Engine protocol predict request returned 400: {\"error\":{\"code\":400,\"message\":\"request (41972 tokens) exceeds the available context size (32000 tokens)\"}}"}}`
	srv := sseServer(t, 200, `{"choices":[{"delta":{"content":"a"}}]}`, overflow)
	defer srv.Close()
	_, _, err := New(srv.URL).Stream(context.Background(), Request{Model: "m"}, nil)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 400 || Retryable(err) {
		t.Fatalf("context overflow must be a non-retryable 400, got %v", err)
	}
	for _, c := range []struct {
		code any
		msg  string
		want int
	}{
		{float64(429), "slow down", 429},
		{nil, "request exceeds the available context size", 400},
		{nil, `Engine protocol predict stream returned an error: {"code":500,"message":"Context size has been exceeded.","type":"server_error"}`, 500},
		{nil, `Engine protocol predict request returned 400: {"error":{"code":400,"message":"request (41846 tokens) exceeds the available context size (32000 tokens)"}}`, 400},
		{"x", "model crashed", 502},
	} {
		if got := streamErrorCode(c.code, c.msg); got != c.want {
			t.Errorf("streamErrorCode(%v, %q) = %d, want %d", c.code, c.msg, got, c.want)
		}
	}
}

func TestContextPressureIsTransient(t *testing.T) {
	full := `{"error":{"message":"Engine protocol predict stream returned an error: {\"code\":500,\"message\":\"Context size has been exceeded.\",\"type\":\"server_error\"}"}}`
	srv := sseServer(t, 200, `{"choices":[{"delta":{"content":"a"}}]}`, full)
	defer srv.Close()
	_, _, err := New(srv.URL).Stream(context.Background(), Request{Model: "m"}, nil)
	if !ContextPressure(err) || !Retryable(err) {
		t.Fatalf("shared-context exhaustion must be transient: %v", err)
	}
	if ContextPressure(&StatusError{Code: 400, Body: "request (41846 tokens) exceeds the available context size"}) {
		t.Fatal("a too-large request is not context pressure")
	}
}
