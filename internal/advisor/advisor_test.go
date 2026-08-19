package advisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrybas/tmux-agent-hub/internal/state"
)

func TestLoadTemplatesEmbedded(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	templates := LoadTemplates()
	names := map[string]bool{}
	for _, tpl := range templates {
		names[tpl.Name] = true
	}
	for _, want := range []string{"review", "check-diff", "write-tests"} {
		if !names[want] {
			t.Errorf("embedded template %q missing (have %v)", want, names)
		}
	}
}

func TestLoadTemplatesUserOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	tplDir := filepath.Join(dir, "tmux-agent-hub", "templates")
	os.MkdirAll(tplDir, 0o755)
	os.WriteFile(filepath.Join(tplDir, "review.md"), []byte("my own review {{output}}"), 0o644)
	os.WriteFile(filepath.Join(tplDir, "custom.md"), []byte("custom prompt"), 0o644)

	tpl, ok := FindTemplate("review")
	if !ok || tpl.Body != "my own review {{output}}" {
		t.Errorf("user template must shadow embedded, got %+v", tpl)
	}
	if _, ok := FindTemplate("custom"); !ok {
		t.Error("user-added template not listed")
	}
}

func TestBuildMessage(t *testing.T) {
	tpl := Template{Name: "t", Body: "In {{cwd}} the agent {{agent}} said:\n{{output}}"}
	src := &state.Pane{Agent: "claude", Cwd: "/repo/x"}
	msg := BuildMessage(tpl, src, "the reply", 8000)
	if !strings.Contains(msg, "In /repo/x the agent claude said:\nthe reply") {
		t.Errorf("placeholders not replaced: %q", msg)
	}
	if !strings.HasSuffix(msg, "\n") {
		t.Error("message must end with a newline")
	}

	msg = BuildMessage(tpl, nil, "", 8000)
	if !strings.Contains(msg, "(no output captured)") || !strings.Contains(msg, "(unknown)") {
		t.Errorf("nil source fallbacks missing: %q", msg)
	}
}

func TestNeedsOutput(t *testing.T) {
	if !(Template{Body: "x {{output}}"}).NeedsOutput() {
		t.Error("template with {{output}} must need output")
	}
	if (Template{Body: "check the diff in {{cwd}}"}).NeedsOutput() {
		t.Error("template without {{output}} must not need output")
	}
}

func TestCustomTemplate(t *testing.T) {
	tpl := CustomTemplate("please review this carefully")
	if !tpl.NeedsOutput() {
		t.Error("custom template without {{output}} must get it appended")
	}
	msg := BuildMessage(tpl, &state.Pane{Agent: "claude", Cwd: "/x"}, "the reply", 8000)
	if !strings.Contains(msg, "please review this carefully") || !strings.Contains(msg, "the reply") {
		t.Errorf("custom message wrong: %q", msg)
	}

	tpl = CustomTemplate("summarize {{output}} briefly")
	if strings.Contains(tpl.Body, "--- output of") {
		t.Error("explicit {{output}} must not get the appendix")
	}
}
