// Package skills answers "what does this agent see and where does it come
// from": skills, slash commands, subagents, MCP servers and memory files,
// each labeled with its source level (global / project / plugin).
package skills

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Item struct {
	Name   string
	Source string // "global" | "project <dir>" | "plugin <name>"
	Path   string // the file to open when editing this item
}

type Report struct {
	Agent    string
	Cwd      string
	Memory   []string // CLAUDE.md / AGENTS.md chain, in load order
	Skills   []Item
	Commands []Item
	Agents   []Item
	MCP      []Item
}

// Inspect builds the report for an agent working in cwd. home is the
// user's home directory (parameterized for tests).
func Inspect(home, cwd, agent string) *Report {
	r := &Report{Agent: agent, Cwd: cwd}
	if agent == "codex" {
		inspectCodex(r, home, cwd)
		return r
	}
	inspectClaude(r, home, cwd)
	return r
}

func inspectClaude(r *Report, home, cwd string) {
	global := filepath.Join(home, ".claude")

	// memory chain: global first, then top-most parent down to cwd
	if p := filepath.Join(global, "CLAUDE.md"); exists(p) {
		r.Memory = append(r.Memory, p)
	}
	for _, dir := range chain(home, cwd) {
		for _, name := range []string{"CLAUDE.md", "CLAUDE.local.md"} {
			if p := filepath.Join(dir, name); exists(p) {
				r.Memory = append(r.Memory, p)
			}
		}
	}

	collect := func(base, source string) {
		r.Skills = append(r.Skills, scanSkills(filepath.Join(base, "skills"), source)...)
		r.Commands = append(r.Commands, scanMarkdown(filepath.Join(base, "commands"), source)...)
		r.Agents = append(r.Agents, scanMarkdown(filepath.Join(base, "agents"), source)...)
	}
	collect(global, "global")
	for _, dir := range chain(home, cwd) {
		if sub := filepath.Join(dir, ".claude"); exists(sub) {
			collect(sub, "project "+short(home, dir))
		}
	}
	for name, installPath := range enabledPlugins(home) {
		collect(installPath, "plugin "+name)
	}

	// MCP: ~/.claude.json (global + per-project) and .mcp.json files
	r.MCP = append(r.MCP, claudeJSONMCP(home, cwd)...)
	for _, dir := range chain(home, cwd) {
		mcpFile := filepath.Join(dir, ".mcp.json")
		if names := mcpJSONServers(mcpFile); len(names) > 0 {
			for _, n := range names {
				r.MCP = append(r.MCP, Item{Name: n, Source: "project " + short(home, dir), Path: mcpFile})
			}
		}
	}
	sortItems(r)
}

func inspectCodex(r *Report, home, cwd string) {
	codex := filepath.Join(home, ".codex")
	if p := filepath.Join(codex, "AGENTS.md"); exists(p) {
		r.Memory = append(r.Memory, p)
	}
	for _, dir := range chain(home, cwd) {
		if p := filepath.Join(dir, "AGENTS.md"); exists(p) {
			r.Memory = append(r.Memory, p)
		}
	}
	r.Skills = append(r.Skills, scanSkills(filepath.Join(codex, "skills"), "global")...)
	// skills that ship with Codex itself live in a hidden subdirectory
	r.Skills = append(r.Skills, scanSkills(filepath.Join(codex, "skills", ".system"), "system")...)
	for _, dir := range chain(home, cwd) {
		r.Skills = append(r.Skills, scanSkills(filepath.Join(dir, ".codex", "skills"),
			"project "+short(home, dir))...)
	}

	var cfg struct {
		MCPServers map[string]any `toml:"mcp_servers"`
		Plugins    map[string]struct {
			Enabled bool `toml:"enabled"`
		} `toml:"plugins"`
	}
	codexCfg := filepath.Join(codex, "config.toml")
	if _, err := toml.DecodeFile(codexCfg, &cfg); err == nil {
		// MCP servers from [mcp_servers]
		for name := range cfg.MCPServers {
			r.MCP = append(r.MCP, Item{Name: name, Source: "global", Path: codexCfg})
		}
		// enabled plugins bring their own skills, unpacked under
		// ~/.codex/plugins/cache/<source>/<plugin>/<version>/skills/
		for key, plugin := range cfg.Plugins {
			name, source, ok := strings.Cut(key, "@")
			if !ok || !plugin.Enabled {
				continue
			}
			// the cache keeps every version ever installed ("1.0.20",
			// "11c74d6b", …) — only the newest one is what the agent loads
			versions, _ := filepath.Glob(filepath.Join(codex, "plugins", "cache", source, name, "*"))
			if version := newest(versions); version != "" {
				r.Skills = append(r.Skills,
					scanSkills(filepath.Join(version, "skills"), "plugin "+name)...)
			}
		}
	}
	sortItems(r)
}

// newest picks the most recently modified directory — version names mix
// semver and content hashes, so mtime is the only order they share.
func newest(paths []string) string {
	var best string
	var bestAt time.Time
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil || !st.IsDir() {
			continue
		}
		if best == "" || st.ModTime().After(bestAt) {
			best, bestAt = p, st.ModTime()
		}
	}
	return best
}

// chain lists directories from the top-most parent below home (or root)
// down to cwd — the order Claude Code loads project files in.
func chain(home, cwd string) []string {
	var dirs []string
	dir := filepath.Clean(cwd)
	for {
		if dir == home || dir == "/" || dir == "." {
			break
		}
		dirs = append([]string{dir}, dirs...)
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return dirs
}

// scanSkills lists subdirectories containing SKILL.md.
func scanSkills(dir, source string) []Item {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var items []Item
	for _, e := range entries {
		if p := filepath.Join(dir, e.Name(), "SKILL.md"); e.IsDir() && exists(p) {
			items = append(items, Item{Name: e.Name(), Source: source, Path: p})
		}
	}
	return items
}

// scanMarkdown lists *.md files (recursively, "dir:name" for nesting).
func scanMarkdown(dir, source string) []Item {
	var items []Item
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}
		name := strings.TrimSuffix(rel, ".md")
		name = strings.ReplaceAll(name, string(filepath.Separator), ":")
		items = append(items, Item{Name: name, Source: source, Path: path})
		return nil
	})
	return items
}

// enabledPlugins maps enabled plugin names to their install paths.
func enabledPlugins(home string) map[string]string {
	enabled := map[string]bool{}
	var settings struct {
		EnabledPlugins map[string]bool `json:"enabledPlugins"`
	}
	if data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json")); err == nil {
		if json.Unmarshal(data, &settings) == nil {
			enabled = settings.EnabledPlugins
		}
	}
	var installed struct {
		Plugins map[string][]struct {
			InstallPath string `json:"installPath"`
		} `json:"plugins"`
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude", "plugins", "installed_plugins.json"))
	if err != nil || json.Unmarshal(data, &installed) != nil {
		return nil
	}
	out := map[string]string{}
	for name, versions := range installed.Plugins {
		if !enabled[name] || len(versions) == 0 {
			continue
		}
		// display name without the marketplace suffix
		display, _, _ := strings.Cut(name, "@")
		out[display] = versions[len(versions)-1].InstallPath
	}
	return out
}

func claudeJSONMCP(home, cwd string) []Item {
	var cfg struct {
		MCPServers map[string]any `json:"mcpServers"`
		Projects   map[string]struct {
			MCPServers map[string]any `json:"mcpServers"`
		} `json:"projects"`
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil || json.Unmarshal(data, &cfg) != nil {
		return nil
	}
	cfgPath := filepath.Join(home, ".claude.json")
	var items []Item
	for name := range cfg.MCPServers {
		items = append(items, Item{Name: name, Source: "global", Path: cfgPath})
	}
	if proj, ok := cfg.Projects[cwd]; ok {
		for name := range proj.MCPServers {
			items = append(items, Item{Name: name, Source: "project " + short(home, cwd), Path: cfgPath})
		}
	}
	return items
}

func mcpJSONServers(path string) []string {
	var cfg struct {
		MCPServers map[string]any `json:"mcpServers"`
	}
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &cfg) != nil {
		return nil
	}
	var names []string
	for name := range cfg.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortItems(r *Report) {
	for _, list := range [][]Item{r.Skills, r.Commands, r.Agents, r.MCP} {
		sort.Slice(list, func(i, j int) bool {
			if list[i].Source != list[j].Source {
				return list[i].Source < list[j].Source
			}
			return list[i].Name < list[j].Name
		})
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func short(home, p string) string {
	if p == home {
		return "~"
	}
	if strings.HasPrefix(p, home+"/") {
		return "~" + p[len(home):]
	}
	return p
}

// Line is one row of the rendered report. Rows with a Path can be opened
// in an editor.
type Line struct {
	Text   string
	Path   string
	Header bool
}

// Rows renders the report as structured lines; headers start sections.
func (r *Report) Rows(home string) []Line {
	var rows []Line
	text := func(format string, args ...any) { rows = append(rows, Line{Text: fmt.Sprintf(format, args...)}) }
	header := func(t string) {
		rows = append(rows, Line{}, Line{Text: t, Header: true})
	}
	item := func(t, path string) { rows = append(rows, Line{Text: t, Path: path}) }

	text("agent %s · %s", r.Agent, short(home, r.Cwd))
	memName := "CLAUDE.md"
	if r.Agent == "codex" {
		memName = "AGENTS.md"
	}
	header("memory (" + memName + ")")
	if len(r.Memory) == 0 {
		text("  (none)")
	}
	for _, m := range r.Memory {
		item("  "+short(home, m), m)
	}
	section := func(title string, items []Item) {
		header(title)
		if len(items) == 0 {
			text("  (none)")
			return
		}
		width := 0
		for _, it := range items {
			if n := len([]rune(it.Name)); n > width {
				width = n
			}
		}
		for _, it := range items {
			item(fmt.Sprintf("  %-*s  %s", width, it.Name, it.Source), it.Path)
		}
	}
	section("skills", r.Skills)
	section("commands", r.Commands)
	section("agents", r.Agents)
	section("mcp servers", r.MCP)
	return rows
}

// Lines is the plain-text rendering (CLI).
func (r *Report) Lines(home string) []string {
	var lines []string
	for _, row := range r.Rows(home) {
		if row.Header {
			lines = append(lines, "# "+row.Text)
		} else {
			lines = append(lines, row.Text)
		}
	}
	return lines
}
