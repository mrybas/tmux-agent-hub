// Package layout makes hotkeys work in Cyrillic keyboard layouts: the
// same physical key sends "у" instead of "e" under ЙЦУКЕН, so tmux
// bindings get Cyrillic duplicates and TUIs normalize incoming runes.
package layout

// qwertyToCyr maps a Latin key to what the same physical key produces in
// Ukrainian/Russian ЙЦУКЕН layouts (both variants where they differ).
var qwertyToCyr = map[rune][]rune{
	'q': {'й'}, 'w': {'ц'}, 'e': {'у'}, 'r': {'к'}, 't': {'е'},
	'y': {'н'}, 'u': {'г'}, 'i': {'ш'}, 'o': {'щ'}, 'p': {'з'},
	'a': {'ф'}, 's': {'і', 'ы'}, 'd': {'в'}, 'f': {'а'}, 'g': {'п'},
	'h': {'р'}, 'j': {'о'}, 'k': {'л'}, 'l': {'д'},
	'z': {'я'}, 'x': {'ч'}, 'c': {'с'}, 'v': {'м'}, 'b': {'и'},
	'n': {'т'}, 'm': {'ь'},
}

var cyrToLatin = func() map[rune]rune {
	m := map[rune]rune{}
	for lat, cyrs := range qwertyToCyr {
		for _, c := range cyrs {
			m[c] = lat
			m[upper(c)] = upper(lat)
		}
	}
	return m
}()

func upper(r rune) rune {
	switch {
	case r >= 'a' && r <= 'z':
		return r - 32
	case r >= 'а' && r <= 'я':
		return r - 32
	case r == 'і':
		return 'І'
	case r == 'ы':
		return 'Ы'
	case r == 'й':
		return 'Й'
	}
	return r
}

// Alts returns the other characters living on the same physical key (for
// duplicating tmux bindings): Cyrillic twins for a Latin key, and the
// Latin twin (plus sibling Cyrillic variants) for a Cyrillic key — so a
// custom Cyrillic key in the config works from the English layout too.
// Non-letter and multi-rune keys have none.
func Alts(key string) []string {
	r := []rune(key)
	if len(r) != 1 {
		return nil
	}
	base := r[0]

	// Cyrillic key: find its Latin twin, return it plus sibling variants
	if lat, ok := cyrToLatin[base]; ok {
		alts := []string{string(lat)}
		lower := lat
		shifted := lat >= 'A' && lat <= 'Z'
		if shifted {
			lower = lat + 32
		}
		for _, c := range qwertyToCyr[lower] {
			if shifted {
				c = upper(c)
			}
			if c != base {
				alts = append(alts, string(c))
			}
		}
		return alts
	}

	lower := base
	shifted := base >= 'A' && base <= 'Z'
	if shifted {
		lower = base + 32
	}
	var alts []string
	for _, c := range qwertyToCyr[lower] {
		if shifted {
			c = upper(c)
		}
		alts = append(alts, string(c))
	}
	return alts
}

// Normalize translates a single Cyrillic rune key back to its physical
// QWERTY equivalent; anything else (Latin, "enter", "ctrl+c") passes
// through unchanged.
func Normalize(key string) string {
	r := []rune(key)
	if len(r) != 1 {
		return key
	}
	if lat, ok := cyrToLatin[r[0]]; ok {
		return string(lat)
	}
	return key
}
