// Package discovery lists the models an inference host offers. LM Studio's
// native API (/api/v1/models) is preferred because it reports size,
// quantization, load state, parallel slots and reasoning support; any other
// OpenAI-compatible server falls back to /v1/models (ids only). For LM Studio
// it can also load and unload models, which matters on a single GPU where two
// large models can't be resident at once.
package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	KindLMStudio = "lmstudio"
	KindOpenAI   = "openai"
)

type Instance struct {
	ID            string `json:"id"`
	ContextLength int    `json:"context_length,omitempty"`
	Parallel      int    `json:"parallel,omitempty"`
	TTLSeconds    *int   `json:"ttl_seconds,omitempty"`
}

type Model struct {
	ID          string     `json:"id"`
	Type        string     `json:"type,omitempty"` // llm | embedding | ...
	DisplayName string     `json:"display_name,omitempty"`
	Publisher   string     `json:"publisher,omitempty"`
	Arch        string     `json:"arch,omitempty"`
	Params      string     `json:"params,omitempty"`
	Quant       string     `json:"quant,omitempty"`
	SizeBytes   int64      `json:"size_bytes,omitempty"`
	MaxContext  int        `json:"max_context,omitempty"`
	Loaded      bool       `json:"loaded"`
	Instances   []Instance `json:"instances,omitempty"`
	Reasoning   []string   `json:"reasoning,omitempty"` // allowed options, e.g. ["off","on"]
	Vision      bool       `json:"vision,omitempty"`
	ToolUse     bool       `json:"tool_use,omitempty"`
}

type Host struct {
	URL    string  `json:"url"`
	Kind   string  `json:"kind,omitempty"`
	OK     bool    `json:"ok"`
	Error  string  `json:"error,omitempty"`
	Models []Model `json:"models"`
}

type Client struct {
	HTTP *http.Client
}

func New() *Client { return &Client{HTTP: &http.Client{Timeout: 6 * time.Second}} }

// BaseURL normalizes an endpoint ("dns+http://svc:1234/v1/") to "http://svc:1234".
func BaseURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimPrefix(strings.TrimSpace(raw), "dns+"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("discovery: %q is not an http(s) URL", raw)
	}
	return u.Scheme + "://" + u.Host, nil
}

func (c *Client) Discover(ctx context.Context, base string) Host {
	h := Host{URL: base, Models: []Model{}}
	if ms, err := c.lmstudio(ctx, base); err == nil {
		h.Kind, h.OK, h.Models = KindLMStudio, true, ms
		return h
	}
	ms, err := c.openai(ctx, base)
	if err != nil {
		h.Error = err.Error()
		return h
	}
	h.Kind, h.OK, h.Models = KindOpenAI, true, ms
	return h
}

func (c *Client) getJSON(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

type lmModels struct {
	Models []struct {
		Type         string `json:"type"`
		Publisher    string `json:"publisher"`
		Key          string `json:"key"`
		DisplayName  string `json:"display_name"`
		Architecture string `json:"architecture"`
		Quantization *struct {
			Name string `json:"name"`
		} `json:"quantization"`
		SizeBytes       int64  `json:"size_bytes"`
		ParamsString    string `json:"params_string"`
		MaxContext      int    `json:"max_context_length"`
		LoadedInstances []struct {
			ID     string `json:"id"`
			Config struct {
				ContextLength int `json:"context_length"`
				Parallel      int `json:"parallel"`
			} `json:"config"`
			RemainingTTL *int `json:"remaining_ttl_seconds"`
		} `json:"loaded_instances"`
		Capabilities struct {
			Vision    bool `json:"vision"`
			ToolUse   bool `json:"trained_for_tool_use"`
			Reasoning *struct {
				Allowed []string `json:"allowed_options"`
			} `json:"reasoning"`
		} `json:"capabilities"`
	} `json:"models"`
}

func (c *Client) lmstudio(ctx context.Context, base string) ([]Model, error) {
	var d lmModels
	if err := c.getJSON(ctx, base+"/api/v1/models", &d); err != nil {
		return nil, err
	}
	if d.Models == nil {
		return nil, errors.New("not an LM Studio v1 response")
	}
	out := make([]Model, 0, len(d.Models))
	for _, m := range d.Models {
		mm := Model{
			ID: m.Key, Type: m.Type, DisplayName: m.DisplayName, Publisher: m.Publisher, Arch: m.Architecture,
			Params: m.ParamsString, SizeBytes: m.SizeBytes, MaxContext: m.MaxContext,
			Loaded: len(m.LoadedInstances) > 0, Vision: m.Capabilities.Vision, ToolUse: m.Capabilities.ToolUse,
		}
		if m.Quantization != nil {
			mm.Quant = m.Quantization.Name
		}
		if m.Capabilities.Reasoning != nil {
			mm.Reasoning = m.Capabilities.Reasoning.Allowed
		}
		for _, in := range m.LoadedInstances {
			mm.Instances = append(mm.Instances, Instance{ID: in.ID, ContextLength: in.Config.ContextLength, Parallel: in.Config.Parallel, TTLSeconds: in.RemainingTTL})
		}
		out = append(out, mm)
	}
	return out, nil
}

func (c *Client) openai(ctx context.Context, base string) ([]Model, error) {
	var d struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, base+"/v1/models", &d); err != nil {
		return nil, err
	}
	out := make([]Model, 0, len(d.Data))
	for _, m := range d.Data {
		// Servers listing via /v1/models only expose models they can serve now.
		out = append(out, Model{ID: m.ID, Loaded: true})
	}
	return out, nil
}

type LoadResult struct {
	InstanceID      string  `json:"instance_id"`
	LoadTimeSeconds float64 `json:"load_time_seconds"`
}

// Load asks an LM Studio host to load a model. Loading can take minutes, so
// callers should pass a generous context deadline.
func (c *Client) Load(ctx context.Context, base, model string, contextLength int) (*LoadResult, error) {
	body := map[string]any{"model": model}
	if contextLength > 0 {
		body["context_length"] = contextLength
	}
	var res LoadResult
	if err := c.post(ctx, base+"/api/v1/models/load", body, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (c *Client) Unload(ctx context.Context, base, instanceID string) error {
	return c.post(ctx, base+"/api/v1/models/unload", map[string]any{"instance_id": instanceID}, nil)
}

func (c *Client) post(ctx context.Context, u string, body, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// Load requests outlive the default discovery timeout.
	hc := &http.Client{Transport: c.HTTP.Transport}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error any `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != nil {
			return fmt.Errorf("%s: %v", resp.Status, e.Error)
		}
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}
