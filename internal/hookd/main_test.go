package hookd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mrybas/tmux-agent-hub/internal/config"
	"github.com/mrybas/tmux-agent-hub/internal/eventlog"
)

// TestMain isolates every test in this package from the user's real
// environment: hook handlers fire tmux commands (SendText, refresh) that
// must never reach live panes, and they read the config, which must be a
// fixture rather than whatever the developer happens to run with.
func TestMain(m *testing.M) {
	os.Setenv("TMUX_AGENT_HUB_TEST_SOCKET", "tmux-agent-hub-hookd-test")
	os.Unsetenv("TMUX")

	// the advisor log lives under XDG_STATE_HOME — without this the suite
	// appends fake reviews to the user's real log
	stateHome, err := os.MkdirTemp("", "hub-test-state")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(stateHome)
	os.Setenv("XDG_STATE_HOME", stateHome)

	cfgHome, err := os.MkdirTemp("", "hub-test-config")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(cfgHome)
	os.Setenv("XDG_CONFIG_HOME", cfgHome)
	if err := os.MkdirAll(filepath.Join(cfgHome, "tmux-agent-hub"), 0o755); err != nil {
		panic(err)
	}
	// no rate limiting and no stalling in tests
	// no rate limiting, no stalling, no waiting on transcripts in tests;
	// the limits under test keep their real defaults
	fixture := advisorFixture(10)
	if err := os.WriteFile(filepath.Join(cfgHome, "tmux-agent-hub", "config.toml"),
		[]byte(fixture), 0o644); err != nil {
		panic(err)
	}
	// hook handlers type into agent panes; there is no tmux server here,
	// so record what would have been sent instead of failing every send
	sendText = func(pane, text string) error {
		sentMu.Lock()
		defer sentMu.Unlock()
		sent = append(sent, sentMessage{Pane: pane, Text: text})
		return sentErr
	}
	os.Exit(m.Run())
}

// sentMessage is one recorded send from a hook handler.
type sentMessage struct{ Pane, Text string }

var (
	sentMu  sync.Mutex
	sent    []sentMessage
	sentErr error // when set, every send fails — the pane is gone
)

// resetSent clears the record and the failure mode; call it from any test
// that inspects what was sent.
func resetSent(t *testing.T) {
	t.Helper()
	sentMu.Lock()
	sent, sentErr = nil, nil
	sentMu.Unlock()
	t.Cleanup(func() {
		sentMu.Lock()
		sent, sentErr = nil, nil
		sentMu.Unlock()
	})
}

func failSends(t *testing.T, err error) {
	t.Helper()
	resetSent(t)
	sentMu.Lock()
	sentErr = err
	sentMu.Unlock()
}

// sentTo returns the messages typed into one pane.
func sentTo(pane string) []string {
	sentMu.Lock()
	defer sentMu.Unlock()
	var out []string
	for _, m := range sent {
		if m.Pane == pane {
			out = append(out, m.Text)
		}
	}
	return out
}

// resetAdvisorLog empties the advisor log so a test can assert on exactly
// the events it produced. The log lives under the temp XDG_STATE_HOME set
// above; tests in this package do not run in parallel.
func resetAdvisorLog(t *testing.T) {
	t.Helper()
	if path := eventlog.Path("advisor"); path != "" {
		os.Remove(path)
	}
}

// advisorEvents returns the event names recorded so far, in order.
func advisorEvents(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(eventlog.Path("advisor"))
	if err != nil {
		return nil
	}
	var events []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var rec eventlog.Advice
		if json.Unmarshal([]byte(line), &rec) == nil {
			events = append(events, rec.Event)
		}
	}
	return events
}

func countEvent(events []string, name string) int {
	n := 0
	for _, e := range events {
		if e == name {
			n++
		}
	}
	return n
}

// advisorFixture is the test config, parameterized by the one value some
// tests need to change: how long a boundary waits for the transcript.
func advisorFixture(graceMS int) string {
	return fmt.Sprintf("[advisor]\nmode = \"live\"\nmin_interval = 0\ncatch_up_max = 0\n"+
		"advice_max = 3\nlost_after = 600\nqueue = 8\ngrace_ms = %d\nstale_after = 900\n", graceMS)
}

// withBoundaryGrace rewrites the config for one test. The plugin re-reads
// it on every hook event, so this takes effect immediately.
func withBoundaryGrace(t *testing.T, graceMS int) {
	t.Helper()
	path := config.Path()
	if err := os.WriteFile(path, []byte(advisorFixture(graceMS)), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.WriteFile(path, []byte(advisorFixture(10)), 0o644) })
}

// The limits the tests reason about, read from the same fixture config
// the code reads — a test that hard-codes them tests a different plugin.
func advisorLimits() config.Advisor {
	cfg, err := config.Load()
	if err != nil {
		panic(err)
	}
	return cfg.Advisor
}

var (
	adviceMax  = advisorLimits().AdviceLimit()
	queueLimit = advisorLimits().QueueLimit()
)
