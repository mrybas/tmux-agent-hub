package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInspectClaude(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "repo", "app")

	// global level
	write(t, filepath.Join(home, ".claude", "CLAUDE.md"), "global memory")
	write(t, filepath.Join(home, ".claude", "skills", "deploy", "SKILL.md"), "x")
	write(t, filepath.Join(home, ".claude", "commands", "review.md"), "x")
	// project level: repo has .claude, app has CLAUDE.md
	write(t, filepath.Join(home, "repo", ".claude", "skills", "infra", "SKILL.md"), "x")
	write(t, filepath.Join(cwd, "CLAUDE.md"), "app memory")
	write(t, filepath.Join(cwd, ".claude", "agents", "tester.md"), "x")
	write(t, filepath.Join(cwd, ".mcp.json"), `{"mcpServers":{"jira":{}}}`)
	// enabled plugin with a skill; disabled plugin must be ignored
	write(t, filepath.Join(home, ".claude", "settings.json"),
		`{"enabledPlugins":{"atlassian@official":true,"unused@official":false}}`)
	plug := filepath.Join(home, ".claude", "plugins", "cache", "atlassian")
	write(t, filepath.Join(plug, "skills", "pages", "SKILL.md"), "x")
	unused := filepath.Join(home, ".claude", "plugins", "cache", "unused")
	write(t, filepath.Join(unused, "skills", "nope", "SKILL.md"), "x")
	write(t, filepath.Join(home, ".claude", "plugins", "installed_plugins.json"), `{
	  "plugins": {
	    "atlassian@official": [{"installPath": "`+plug+`"}],
	    "unused@official": [{"installPath": "`+unused+`"}]
	  }}`)
	// global + project mcp in ~/.claude.json
	write(t, filepath.Join(home, ".claude.json"),
		`{"mcpServers":{"chrome":{}},"projects":{"`+cwd+`":{"mcpServers":{"drive":{}}}}}`)

	r := Inspect(home, cwd, "claude")

	if len(r.Memory) != 2 ||
		!strings.HasSuffix(r.Memory[0], ".claude/CLAUDE.md") ||
		!strings.HasSuffix(r.Memory[1], "app/CLAUDE.md") {
		t.Errorf("memory chain wrong: %v", r.Memory)
	}
	wantSkills := map[string]string{"deploy": "global", "infra": "project ~/repo", "pages": "plugin atlassian"}
	if len(r.Skills) != len(wantSkills) {
		t.Fatalf("skills = %+v", r.Skills)
	}
	for _, it := range r.Skills {
		if wantSkills[it.Name] != it.Source {
			t.Errorf("skill %s source = %q, want %q", it.Name, it.Source, wantSkills[it.Name])
		}
	}
	if len(r.Commands) != 1 || r.Commands[0].Name != "review" || r.Commands[0].Source != "global" {
		t.Errorf("commands = %+v", r.Commands)
	}
	if len(r.Agents) != 1 || r.Agents[0].Name != "tester" || !strings.Contains(r.Agents[0].Source, "repo/app") {
		t.Errorf("agents = %+v", r.Agents)
	}
	mcp := map[string]bool{}
	for _, it := range r.MCP {
		mcp[it.Name] = true
	}
	for _, want := range []string{"chrome", "drive", "jira"} {
		if !mcp[want] {
			t.Errorf("mcp missing %s: %+v", want, r.MCP)
		}
	}

	lines := strings.Join(r.Lines(home), "\n")
	for _, want := range []string{"# skills", "deploy", "plugin atlassian", "# mcp servers", "~/repo/app/CLAUDE.md"} {
		if !strings.Contains(lines, want) {
			t.Errorf("Lines missing %q:\n%s", want, lines)
		}
	}
}

func TestInspectCodex(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "proj")
	write(t, filepath.Join(home, ".codex", "AGENTS.md"), "global")
	write(t, filepath.Join(cwd, "AGENTS.md"), "proj")
	write(t, filepath.Join(home, ".codex", "skills", "pdf", "SKILL.md"), "x")
	// what Codex ships with, hidden one level down
	write(t, filepath.Join(home, ".codex", "skills", ".system", "imagegen", "SKILL.md"), "x")
	// an enabled plugin's skills, unpacked in the plugin cache
	// an older version stays in the cache after an update: it must not
	// show up next to (or instead of) the current one
	write(t, filepath.Join(home, ".codex", "plugins", "cache", "openai-curated",
		"slack", "0a11bb22", "skills", "slack-gone", "SKILL.md"), "x")
	write(t, filepath.Join(home, ".codex", "plugins", "cache", "openai-curated",
		"slack", "11c74d6b", "skills", "slack-digest", "SKILL.md"), "x")
	touchLater(t, filepath.Join(home, ".codex", "plugins", "cache", "openai-curated",
		"slack", "11c74d6b"))
	// a disabled plugin must stay invisible
	write(t, filepath.Join(home, ".codex", "plugins", "cache", "openai-curated",
		"jira", "1.0.0", "skills", "jira", "SKILL.md"), "x")
	write(t, filepath.Join(cwd, ".codex", "skills", "house-style", "SKILL.md"), "x")
	write(t, filepath.Join(home, ".codex", "config.toml"),
		"[mcp_servers.github]\ncommand = \"x\"\n\n"+
			"[plugins.\"slack@openai-curated\"]\nenabled = true\n\n"+
			"[plugins.\"jira@openai-curated\"]\nenabled = false\n")

	r := Inspect(home, cwd, "codex")
	if len(r.Memory) != 2 {
		t.Errorf("memory = %v", r.Memory)
	}
	bySource := map[string]string{}
	for _, item := range r.Skills {
		bySource[item.Name] = item.Source
	}
	for name, wantSource := range map[string]string{
		"pdf":          "global",
		"imagegen":     "system",
		"slack-digest": "plugin slack",
		"house-style":  "project ~/proj",
	} {
		if got := bySource[name]; got != wantSource {
			t.Errorf("skill %q source = %q, want %q (all: %+v)", name, got, wantSource, r.Skills)
		}
	}
	if _, listed := bySource["jira"]; listed {
		t.Error("a disabled plugin's skills must not be listed")
	}
	if _, listed := bySource["slack-gone"]; listed {
		t.Error("a superseded plugin version must not be listed")
	}
	if len(r.MCP) != 1 || r.MCP[0].Name != "github" {
		t.Errorf("mcp = %+v", r.MCP)
	}
}

// touchLater makes dir the newest one in its parent.
func touchLater(t *testing.T, dir string) {
	t.Helper()
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(dir, later, later); err != nil {
		t.Fatal(err)
	}
}
