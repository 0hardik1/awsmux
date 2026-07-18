package core

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// AGENT CONTRACT (store): implement every stub in this file. Keep the
// signatures exactly as written; add unexported helpers in this file freely.

// Dir returns the awsmux state directory: $AWSMUX_HOME if set, else
// ~/.awsmux. Creates it (0700) with plans/ and executions/ subdirs.
func Dir() (string, error) {
	dir := os.Getenv("AWSMUX_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		dir = filepath.Join(home, ".awsmux")
	}
	for _, d := range []string{dir, filepath.Join(dir, "plans"), filepath.Join(dir, "executions")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return "", fmt.Errorf("create state directory: %w", err)
		}
	}
	return dir, nil
}

// SavePlan writes the plan to <Dir()>/plans/<id>.json (0600, indented).
func SavePlan(p *Plan) error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := writeJSONAtomic(filepath.Join(dir, "plans", p.ID+".json"), p); err != nil {
		return fmt.Errorf("save plan %s: %w", p.ID, err)
	}
	return nil
}

// LoadPlan reads a plan by ID. A missing plan is an error that includes the
// ID. Accept unambiguous ID prefixes (agents truncate IDs constantly).
func LoadPlan(id string) (*Plan, error) {
	path, err := resolveJSONFile("plans", "plan", id)
	if err != nil {
		return nil, err
	}
	var p Plan
	if err := readJSON(path, &p); err != nil {
		return nil, fmt.Errorf("load plan %s: %w", id, err)
	}
	return &p, nil
}

// ListPlans returns all plans, newest first.
func ListPlans() ([]*Plan, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(dir, "plans"))
	if err != nil {
		return nil, fmt.Errorf("list plans: %w", err)
	}
	plans := make([]*Plan, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var p Plan
		if err := readJSON(filepath.Join(dir, "plans", e.Name()), &p); err != nil {
			continue // skip unreadable files
		}
		plans = append(plans, &p)
	}
	sort.Slice(plans, func(i, j int) bool {
		return plans[i].CreatedAt.After(plans[j].CreatedAt)
	})
	return plans, nil
}

// executionIndexEntry is one summary line in executions/index.jsonl.
type executionIndexEntry struct {
	ID             string         `json:"id"`
	PlanID         string         `json:"plan_id,omitempty"`
	Service        string         `json:"service"`
	Operation      string         `json:"operation"`
	Classification Classification `json:"classification"`
	StartedAt      time.Time      `json:"started_at"`
	Summary        Summary        `json:"summary"`
}

// SaveExecution writes <Dir()>/executions/<id>.json and appends one summary
// line {id, plan_id, service, operation, classification, started_at,
// summary} to <Dir()>/executions/index.jsonl. Saving the same execution ID
// again rewrites the JSON but must not duplicate the index line.
func SaveExecution(e *Execution) error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	execDir := filepath.Join(dir, "executions")
	if err := writeJSONAtomic(filepath.Join(execDir, e.ID+".json"), e); err != nil {
		return fmt.Errorf("save execution %s: %w", e.ID, err)
	}
	indexPath := filepath.Join(execDir, "index.jsonl")
	ids, err := readIndexIDs(indexPath)
	if err != nil {
		return fmt.Errorf("save execution %s: %w", e.ID, err)
	}
	for _, id := range ids {
		if id == e.ID {
			return nil
		}
	}
	line, err := json.Marshal(executionIndexEntry{
		ID:             e.ID,
		PlanID:         e.PlanID,
		Service:        e.Service,
		Operation:      e.Operation,
		Classification: e.Classification,
		StartedAt:      e.StartedAt,
		Summary:        e.Summary,
	})
	if err != nil {
		return fmt.Errorf("save execution %s: encode index entry: %w", e.ID, err)
	}
	f, err := os.OpenFile(indexPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("save execution %s: open index: %w", e.ID, err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("save execution %s: append index: %w", e.ID, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("save execution %s: close index: %w", e.ID, err)
	}
	return nil
}

// LoadExecution reads an execution by ID (prefix match allowed).
func LoadExecution(id string) (*Execution, error) {
	path, err := resolveJSONFile("executions", "execution", id)
	if err != nil {
		return nil, err
	}
	var e Execution
	if err := readJSON(path, &e); err != nil {
		return nil, fmt.Errorf("load execution %s: %w", id, err)
	}
	return &e, nil
}

// ListExecutions returns full executions, newest first, reading the index
// for order then loading each file (skip unreadable entries).
func ListExecutions() ([]*Execution, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	execDir := filepath.Join(dir, "executions")
	ids, err := readIndexIDs(filepath.Join(execDir, "index.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("list executions: %w", err)
	}
	execs := make([]*Execution, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		var e Execution
		if err := readJSON(filepath.Join(execDir, id+".json"), &e); err != nil {
			continue // skip unreadable entries
		}
		execs = append(execs, &e)
	}
	// The index is append-ordered already, but re-sort to be safe.
	sort.Slice(execs, func(i, j int) bool {
		return execs[i].StartedAt.After(execs[j].StartedAt)
	})
	return execs, nil
}

// readIndexIDs returns the execution IDs from an index.jsonl file, in file
// order. A missing index means no executions, not an error.
func readIndexIDs(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read index: %w", err)
	}
	var ids []string
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var entry struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(line, &entry); err != nil || entry.ID == "" {
			continue // skip corrupt lines
		}
		ids = append(ids, entry.ID)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan index: %w", err)
	}
	return ids, nil
}

// resolveJSONFile finds <Dir()>/<subdir>/<id>.json, falling back to a unique
// ID prefix match. kind ("plan", "execution") is only used in errors.
func resolveJSONFile(subdir, kind, id string) (string, error) {
	if id == "" {
		return "", fmt.Errorf("empty %s id", kind)
	}
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	d := filepath.Join(dir, subdir)
	exact := filepath.Join(d, id+".json")
	if _, err := os.Stat(exact); err == nil {
		return exact, nil
	}
	entries, err := os.ReadDir(d)
	if err != nil {
		return "", fmt.Errorf("read %s directory: %w", kind, err)
	}
	var matches []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		if strings.HasPrefix(name, id) {
			matches = append(matches, strings.TrimSuffix(name, ".json"))
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no %s found with id %q", kind, id)
	case 1:
		return filepath.Join(d, matches[0]+".json"), nil
	default:
		sort.Strings(matches)
		return "", fmt.Errorf("ambiguous id %q: matches %s", id, strings.Join(matches, ", "))
	}
}

// readJSON reads and unmarshals one JSON file.
func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}
	return nil
}

// writeJSONAtomic writes v as indented JSON to path (0600) via a temp file
// and rename, so readers never observe a partial file.
func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encode json: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}
	return nil
}
