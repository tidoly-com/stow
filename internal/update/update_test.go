package update

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		latest, current string
		want            bool
	}{
		{"v0.2.0", "v0.1.0", true},
		{"v0.1.1", "v0.1.0-3-gabc1234-dirty", true}, // some commits after v0.1.0 are still v0.1.0
		{"v0.1.0", "v0.1.0-3-gabc1234", false},
		{"v0.10.0", "v0.9.9", true},
		{"v1.0.0", "v0.99.0", true},
		{"v0.1.0", "v0.1.0", false},
		{"v0.0.9", "v0.1.0", false},
		{"v0.2.0", "dev", false},     // built from the source: not compared
		{"v0.2.0", "0821ff4", false}, // no tag yet
		{"latest", "v0.1.0", false},
		{"", "v0.1.0", false},
	} {
		if got := Newer(c.latest, c.current); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.latest, c.current, got)
		}
	}
}

// The check reads latest.json where the releases are; a build without a version does not ask.
func TestCheck(t *testing.T) {
	asked := 0
	out := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		if r.URL.Path != "/download/latest.json" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"version":"v0.2.0"}`)
	}))
	defer out.Close()
	c := &Checker{Current: "v0.1.0", Base: out.URL + "/download/"}
	if err := c.Check(context.Background()); err != nil || c.Newer() != "v0.2.0" {
		t.Fatalf("check: %v, newer %q", err, c.Newer())
	}
	c.Current = "v0.2.0"
	if c.Newer() != "" {
		t.Error("the same version taken as newer")
	}
	dev := &Checker{Current: "dev", Base: out.URL + "/download"}
	before := asked
	if err := dev.Check(context.Background()); err != nil || asked != before || dev.Newer() != "" {
		t.Errorf("a build from the source asked: %v", err)
	}
	if err := (&Checker{Current: "v0.1.0", Base: out.URL + "/nothing"}).Check(context.Background()); err == nil {
		t.Error("no latest.json, and no error")
	}
}
