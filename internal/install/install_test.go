package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFetchChecksAndSkips(t *testing.T) {
	body := []byte("not really ffmpeg")
	sum := sha256.Sum256(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	defer srv.Close()
	dir := t.TempDir()
	there := filepath.Join(dir, "there")
	os.WriteFile(there, []byte("x"), 0o755)
	parts := []Part{
		{Name: "already", URL: srv.URL + "/x", SHA256: "whatever", Path: there},
		{Name: "ffmpeg", URL: srv.URL + "/ffmpeg", SHA256: hex.EncodeToString(sum[:]), Path: filepath.Join(dir, "bin", "ffmpeg")},
		{Name: "bad", URL: srv.URL + "/bad", SHA256: "00", Path: filepath.Join(dir, "bin", "bad")},
	}
	var p Progress
	Fetch(context.Background(), parts, &p)
	r := p.Report()
	if !r.Finished || r.Error == "" || r.Parts[0].State != "there" || r.Parts[1].State != "done" || r.Parts[2].State != "failed" {
		t.Fatalf("report: %+v", r)
	}
	if r.Parts[1].Got != int64(len(body)) {
		t.Errorf("got %d of %d bytes", r.Parts[1].Got, len(body))
	}
	if info, err := os.Stat(parts[1].Path); err != nil || info.Mode()&0o111 == 0 {
		t.Errorf("ffmpeg not in place and runnable: %v", err)
	}
	if _, err := os.Stat(parts[2].Path); err == nil {
		t.Error("a file with the wrong checksum was put in place")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "bin", "*.part")); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

func TestConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stow", "config.json")
	if _, err := Load(path); err == nil {
		t.Fatal("no file, yet loaded")
	}
	if err := (Config{Data: "/photos"}).Save(path); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(path); err != nil || c.Data != "/photos" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestInvite(t *testing.T) {
	home := Invite("http://192.168.0.10:30891", "mom@example.com", false)
	for _, want := range []string{"Immich", "http://192.168.0.10:30891", "mom@example.com", "비밀번호는 따로", "집 와이파이"} {
		if !strings.Contains(home, want) {
			t.Errorf("home invite lacks %q:\n%s", want, home)
		}
	}
	away := Invite("https://demo.stow.tidoly.app", "", true)
	if strings.Contains(away, "와이파이") || !strings.Contains(away, "Login with tidoly") {
		t.Errorf("tunnel invite:\n%s", away)
	}
}
