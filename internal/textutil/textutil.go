// Package textutil holds small text helpers shared by the TUIs.
package textutil

import (
	"strings"

	"github.com/mattn/go-runewidth"
)

// Wrap breaks s into lines of at most width display cells, preferring word
// boundaries and falling back to hard breaks for long unbroken runs.
// Existing newlines are kept as paragraph breaks.
func Wrap(s string, width int) []string {
	if width < 4 {
		width = 4
	}
	var lines []string
	for _, paragraph := range strings.Split(s, "\n") {
		paragraph = strings.ReplaceAll(paragraph, "\t", "    ")
		line := ""
		for _, word := range strings.Split(paragraph, " ") {
			switch {
			case line == "":
				line = word
			case runewidth.StringWidth(line)+1+runewidth.StringWidth(word) <= width:
				line += " " + word
			default:
				lines = append(lines, line)
				line = word
			}
			// a single word wider than the line: cut it into chunks.
			// The cut counts display cells, not runes — one CJK rune is two
			// cells, so slicing by width would run past the end of the slice.
			for runewidth.StringWidth(line) > width {
				head, rest := cutCells(line, width)
				if head == "" {
					break // a single rune wider than the line: leave it alone
				}
				lines = append(lines, head)
				line = rest
			}
		}
		lines = append(lines, line)
	}
	return lines
}

// cutCells splits s at the last rune that still fits into width display
// cells. Wide runes are never split in half.
func cutCells(s string, width int) (head, rest string) {
	cells := 0
	for i, r := range s {
		w := runewidth.RuneWidth(r)
		if cells+w > width {
			return s[:i], s[i:]
		}
		cells += w
	}
	return s, ""
}
