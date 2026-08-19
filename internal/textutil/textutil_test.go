package textutil

import (
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
)

func TestWrapKeepsLinesWithinWidth(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		width int
	}{
		{"ascii", "the quick brown fox jumps over the lazy dog", 12},
		{"long word", strings.Repeat("x", 40), 9},
		// wide runes: fewer runes than display cells — slicing by rune
		// index used to panic here
		{"cjk", "日本語のテキスト", 10},
		{"cjk long", strings.Repeat("漢", 30), 7},
		{"emoji", strings.Repeat("🙂", 12), 5},
		{"mixed", "fix 日本語 handling in the サイドバー now", 11},
		{"narrower than one rune", "漢字", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lines := Wrap(c.text, c.width) // must not panic
			limit := max(c.width, 4)
			for _, line := range lines {
				if w := runewidth.StringWidth(line); w > limit && len([]rune(line)) > 1 {
					t.Errorf("line %q is %d cells wide, limit %d", line, w, limit)
				}
			}
			if joined := strings.Join(lines, ""); strings.ReplaceAll(joined, " ", "") !=
				strings.ReplaceAll(strings.ReplaceAll(c.text, " ", ""), "\n", "") {
				t.Errorf("wrapping lost or invented text: %q", lines)
			}
		})
	}
}
