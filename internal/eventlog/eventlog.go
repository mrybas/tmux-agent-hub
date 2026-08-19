// Package eventlog appends JSONL records to a rotated file under the
// state directory. It is the measurement substrate for the advisor: what
// was fed to whom, what came back, what the user actually heard.
package eventlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Advice is one advisor event.
type Advice struct {
	Time      time.Time `json:"time"`
	Event     string    `json:"event"` // feed | queued | verdict | hold | deliver | drop | lost | send_failed | unsubmitted | muted | no_verdict | skipped | detect
	Worker    string    `json:"worker"`
	Reviewer  string    `json:"reviewer,omitempty"`
	Agent     string    `json:"agent,omitempty"`     // worker's agent kind
	Reviewing string    `json:"reviewing,omitempty"` // reviewer's agent kind
	Severity  string    `json:"severity,omitempty"`
	Boundary  bool      `json:"boundary,omitempty"` // end-of-turn review
	Note      string    `json:"note,omitempty"`     // advice text, clipped
	Bytes     int       `json:"bytes,omitempty"`    // delta size fed
	LatencyMS int64     `json:"latency_ms,omitempty"`
}

func dir() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "tmux-agent-hub")
}

// Path is the file a named log lives in.
func Path(name string) string {
	d := dir()
	if d == "" {
		return ""
	}
	return filepath.Join(d, name+".log")
}

// Append writes one JSON line, rotating the file once it outgrows maxKB
// (the current file becomes <name>.log.old). Failures are silent: logging
// must never break a hook.
func Append(name string, maxKB int, rec any) {
	path := Path(name)
	if path == "" {
		return
	}
	if maxKB <= 0 {
		maxKB = 1024
	}
	if st, err := os.Stat(path); err == nil && st.Size() > int64(maxKB)*1024 {
		os.Rename(path, path+".old")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	data, err := json.Marshal(rec)
	if err != nil {
		return
	}
	f.Write(append(data, '\n'))
}
