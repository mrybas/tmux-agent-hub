package layout

import "testing"

func TestAlts(t *testing.T) {
	cases := map[string][]string{
		"e":     {"у"},
		"s":     {"і", "ы"},
		"E":     {"У"},
		"o":     {"щ"},
		"enter": nil,
	}
	for key, want := range cases {
		got := Alts(key)
		if len(got) != len(want) {
			t.Errorf("Alts(%q) = %v, want %v", key, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("Alts(%q) = %v, want %v", key, got, want)
			}
		}
	}
}

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"у": "e", "щ": "o", "і": "s", "ы": "s", "о": "j", "л": "k",
		"м": "v", "ш": "i", "ч": "x", "ф": "a", "й": "q", "У": "E",
		"e": "e", "enter": "enter", "ctrl+c": "ctrl+c", "?": "?",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAltsCyrillicKey(t *testing.T) {
	// a Cyrillic config key must gain its Latin twin (and layout siblings)
	got := Alts("ф")
	if len(got) != 1 || got[0] != "a" {
		t.Errorf("Alts(ф) = %v, want [a]", got)
	}
	got = Alts("і") // Ukrainian s-position: latin s + Russian ы
	if len(got) != 2 || got[0] != "s" || got[1] != "ы" {
		t.Errorf("Alts(і) = %v, want [s ы]", got)
	}
	got = Alts("У")
	if len(got) != 1 || got[0] != "E" {
		t.Errorf("Alts(У) = %v, want [E]", got)
	}
}
