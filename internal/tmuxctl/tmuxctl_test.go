package tmuxctl

import "testing"

// Everything the plugin puts in a status-line message comes from outside:
// directory names, tool arguments, an agent's own words. tmux expands
// "#{...}" and "#[...]" in messages, so unescaped text could rewrite the
// toast around it.
func TestEscapeFormat(t *testing.T) {
	cases := map[string]string{
		"plain text":             "plain text",
		"~/repo/#{session_name}": "~/repo/##{session_name}",
		"#[fg=red]fake":          "##[fg=red]fake",
		"go test ./... #(id)":    "go test ./... ##(id)",
		"##already":              "####already",
	}
	for in, want := range cases {
		if got := EscapeFormat(in); got != want {
			t.Errorf("EscapeFormat(%q) = %q, want %q", in, got, want)
		}
	}
}
