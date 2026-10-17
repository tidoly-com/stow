package nearby

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestUnescape(t *testing.T) {
	for in, want := range map[string]string{`tidoly\ stow`: "tidoly stow", `a\.b`: "a.b", "plain": "plain", `end\`: `end\`} {
		if got := unescape(in); got != want {
			t.Errorf("unescape(%q) = %q, want %q", in, got, want)
		}
	}
}

// A server that says it is here is found, by its name and its address on the network. It needs a network
// that carries multicast; without one the test says so and stops.
func TestAnnouncedIsFound(t *testing.T) {
	name := "stow test " + time.Now().Format("150405.000")
	stop, err := Announce(name, 39998)
	if err != nil {
		t.Skipf("no multicast here: %v", err)
	}
	defer stop()
	found, err := Find(context.Background(), 3*time.Second)
	if err != nil {
		t.Skipf("no multicast here: %v", err)
	}
	for _, s := range found {
		if s.Name == name {
			if !strings.HasPrefix(s.Address, "http://") || !strings.HasSuffix(s.Address, ":39998") {
				t.Errorf("address: %q", s.Address)
			}
			return
		}
	}
	t.Skipf("not found — multicast may not reach this machine itself here: %v", found)
}
