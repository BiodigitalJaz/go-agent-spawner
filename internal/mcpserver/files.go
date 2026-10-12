package mcpserver

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// File limits keep one request inside what the API and a local model's
// context can take: a part much larger than a context would fail anyway.
const (
	defaultMaxFiles     = 64
	defaultMaxFileBytes = 120_000 // roughly 30k tokens
	maxTotalBytes       = 6 << 20 // under the API's 8 MiB body limit
)

// skipDirs are never descended into when walking a directory.
var skipDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true, ".venv": true, "__pycache__": true, "dist": true, "build": true, "target": true}

type collected struct {
	Parts   []Part
	Skipped []string
}

// collectFiles turns paths (files, directories or glob patterns) into parts,
// one per readable text file, titled by its path relative to root.
func collectFiles(root string, paths, extensions []string, maxFiles, maxFileBytes int) (*collected, error) {
	if maxFiles <= 0 || maxFiles > defaultMaxFiles {
		maxFiles = defaultMaxFiles
	}
	if maxFileBytes <= 0 {
		maxFileBytes = defaultMaxFileBytes
	}
	var files []string
	seen := map[string]bool{}
	add := func(p string) {
		if abs, err := filepath.Abs(p); err == nil && !seen[abs] {
			seen[abs] = true
			files = append(files, abs)
		}
	}
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		if strings.ContainsAny(p, "*?[") {
			matches, err := filepath.Glob(p)
			if err != nil {
				return nil, fmt.Errorf("bad pattern %q: %w", p, err)
			}
			for _, m := range matches {
				if info, err := os.Stat(m); err == nil && !info.IsDir() {
					add(m)
				}
			}
			continue
		}
		info, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("cannot read %q: %w", p, err)
		}
		if !info.IsDir() {
			add(p)
			continue
		}
		err = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if path != p && (skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
					return filepath.SkipDir
				}
				return nil
			}
			add(path)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	slices.Sort(files)

	out := &collected{}
	total := 0
	for _, f := range files {
		rel, err := filepath.Rel(root, f)
		if err != nil || strings.HasPrefix(rel, "..") {
			rel = f
		}
		if len(extensions) > 0 && !slices.ContainsFunc(extensions, func(e string) bool {
			return strings.EqualFold(filepath.Ext(f), "."+strings.TrimPrefix(e, "."))
		}) {
			continue
		}
		if len(out.Parts) >= maxFiles {
			out.Skipped = append(out.Skipped, rel+" (over the file limit)")
			continue
		}
		data, err := os.ReadFile(f)
		switch {
		case err != nil:
			out.Skipped = append(out.Skipped, rel+" (unreadable)")
			continue
		case len(data) > maxFileBytes:
			out.Skipped = append(out.Skipped, fmt.Sprintf("%s (%d bytes, over the %d-byte limit)", rel, len(data), maxFileBytes))
			continue
		case bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0:
			out.Skipped = append(out.Skipped, rel+" (binary)")
			continue
		case total+len(data) > maxTotalBytes:
			out.Skipped = append(out.Skipped, rel+" (request size limit reached)")
			continue
		}
		total += len(data)
		out.Parts = append(out.Parts, Part{Title: rel, Text: string(data)})
	}
	return out, nil
}
