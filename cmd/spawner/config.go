package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// configFile is what `spawner config export` writes: the saved settings
// (role models, directives, depth...) and the console-managed models. Models
// from backends.json are already in git and are left out.
type configFile struct {
	ExportedAt time.Time        `json:"exported_at"`
	From       string           `json:"from"`
	Version    int              `json:"settings_version"`
	Settings   map[string]any   `json:"settings"`
	Models     []map[string]any `json:"models"`
}

// runConfig keeps the console's configuration in a file so it can live in
// git: every model trial becomes a reviewable diff that can be rolled back.
//
//	spawner config export [file]            write settings and console models (stdout without a file)
//	spawner config import [-dry-run] file   apply a file to the running API
//
// It talks to the API at SPAWNER_URL with the token `spawner mcp` uses.
func runConfig(args []string) error {
	usage := errors.New("usage: spawner config export [file] | spawner config import [-dry-run] <file>")
	if len(args) == 0 {
		return usage
	}
	base := strings.TrimRight(os.Getenv("SPAWNER_URL"), "/")
	if base == "" {
		base = "http://localhost:8080"
	}
	c := apiClient{base: base, auth: apiAuth()}
	switch args[0] {
	case "export":
		if len(args) > 2 {
			return usage
		}
		f, err := c.export()
		if err != nil {
			return err
		}
		b, _ := json.MarshalIndent(f, "", "  ")
		b = append(b, '\n')
		if len(args) == 1 {
			_, err = os.Stdout.Write(b)
			return err
		}
		if err := os.WriteFile(args[1], b, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote %s (settings v%d, %d console models)\n", args[1], f.Version, len(f.Models))
		return nil
	case "import":
		dry := len(args) == 3 && args[1] == "-dry-run"
		if len(args) != 2 && !dry {
			return usage
		}
		b, err := os.ReadFile(args[len(args)-1])
		if err != nil {
			return err
		}
		var want configFile
		if err := json.Unmarshal(b, &want); err != nil {
			return fmt.Errorf("read %s: %w", args[len(args)-1], err)
		}
		return c.importConfig(want, dry)
	}
	return usage
}

type apiClient struct{ base, auth string }

func (c apiClient) do(method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, r)
	if err != nil {
		return err
	}
	if c.auth != "" {
		req.Header.Set("Authorization", c.auth)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// modelFields are the fields a console model is created with.
var modelFields = []string{"name", "model", "endpoints", "initial_limit", "max_limit", "extra", "context_length", "slots", "shared_context", "chunk_tokens"}

func (c apiClient) export() (*configFile, error) {
	var st struct {
		Version  int            `json:"version"`
		Settings map[string]any `json:"settings"`
	}
	if err := c.do("GET", "/v1/settings", nil, &st); err != nil {
		return nil, err
	}
	ms, err := c.consoleModels()
	if err != nil {
		return nil, err
	}
	f := &configFile{ExportedAt: time.Now().UTC().Truncate(time.Second), From: c.base, Version: st.Version, Settings: st.Settings, Models: []map[string]any{}}
	for _, name := range sortedKeys(ms) {
		f.Models = append(f.Models, ms[name])
	}
	return f, nil
}

// consoleModels returns the console-managed models by name, reduced to the
// fields they are created with.
func (c apiClient) consoleModels() (map[string]map[string]any, error) {
	var d struct {
		Models []map[string]any `json:"models"`
	}
	if err := c.do("GET", "/v1/models", nil, &d); err != nil {
		return nil, err
	}
	out := map[string]map[string]any{}
	for _, m := range d.Models {
		if m["source"] != "ui" {
			continue
		}
		keep := map[string]any{}
		for _, k := range modelFields {
			if v, ok := m[k]; ok {
				keep[k] = v
			}
		}
		out[fmt.Sprint(m["name"])] = keep
	}
	return out, nil
}

func (c apiClient) importConfig(want configFile, dry bool) error {
	if want.Settings == nil {
		return errors.New("the file has no settings")
	}
	have, err := c.export()
	if err != nil {
		return err
	}
	current, err := c.consoleModels()
	if err != nil {
		return err
	}
	verb := "applied"
	if dry {
		verb = "would apply"
	}
	// Models first: the settings may name a model the file adds.
	for _, m := range want.Models {
		name := fmt.Sprint(m["name"])
		old, exists := current[name]
		switch {
		case exists && same(old, m):
			continue
		case exists:
			fmt.Printf("%s: update model %s\n", verb, name)
			if !dry {
				if err := c.do("PUT", "/v1/models/"+name, m, nil); err != nil {
					return err
				}
			}
		default:
			fmt.Printf("%s: add model %s\n", verb, name)
			if !dry {
				if err := c.do("POST", "/v1/models", m, nil); err != nil {
					return err
				}
			}
		}
	}
	for name := range current {
		if !containsModel(want.Models, name) {
			fmt.Printf("note: model %s is not in the file; left as it is (delete it in the console if it should go)\n", name)
		}
	}
	changed := diffKeys("", have.Settings, want.Settings)
	if len(changed) == 0 {
		fmt.Println("settings: unchanged")
		return nil
	}
	fmt.Printf("%s: settings changes: %s\n", verb, strings.Join(changed, ", "))
	if dry {
		return nil
	}
	var res struct {
		Version int `json:"version"`
	}
	if err := c.do("PUT", "/v1/settings", map[string]any{"settings": want.Settings, "version": have.Version}, &res); err != nil {
		return err
	}
	fmt.Printf("settings saved as v%d\n", res.Version)
	return nil
}

func same(a, b map[string]any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func containsModel(ms []map[string]any, name string) bool {
	for _, m := range ms {
		if fmt.Sprint(m["name"]) == name {
			return true
		}
	}
	return false
}

// diffKeys lists the dotted keys whose values differ between a and b.
func diffKeys(prefix string, a, b map[string]any) []string {
	var out []string
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		av, bv := a[k], b[k]
		am, aok := av.(map[string]any)
		bm, bok := bv.(map[string]any)
		if aok && bok {
			out = append(out, diffKeys(prefix+k+".", am, bm)...)
			continue
		}
		x, _ := json.Marshal(av)
		y, _ := json.Marshal(bv)
		if !bytes.Equal(x, y) {
			out = append(out, prefix+k)
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
