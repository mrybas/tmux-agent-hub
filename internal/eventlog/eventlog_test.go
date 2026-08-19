package eventlog

import (
	"os"
	"strings"
	"testing"
)

func TestAppendRotates(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path := Path("advisor")

	// one generation is kept, so disk use is capped at ~2x the threshold
	const maxKB = 1
	big := strings.Repeat("x", 400)
	for i := 0; i < 20; i++ {
		Append("advisor", maxKB, Advice{Event: "feed", Worker: "%1", Note: big})
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() > 2*maxKB*1024 {
		t.Errorf("live log grew to %d bytes, rotation did not fire", st.Size())
	}
	old, err := os.Stat(path + ".old")
	if err != nil {
		t.Fatalf("rotated generation missing: %v", err)
	}
	if old.Size() == 0 {
		t.Error("rotated generation is empty")
	}

	// the newest record must still be readable in the live file
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"event":"feed"`) {
		t.Errorf("live log does not contain fresh records: %q", string(data))
	}
}

func TestAppendSurvivesBadState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// must not panic on an unserializable record
	Append("advisor", 1, func() {})
}
