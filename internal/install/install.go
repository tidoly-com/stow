// Package install is what a fresh install needs before the photo server can start (stow issue #18): the
// settings file that says where the photos are kept, and the parts fetched on first start — ffmpeg, and
// later the face models — with how far each has come, for the setup page's progress bar.
package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Config is the settings file. Without a folder, the server shows the setup instead of starting.
type Config struct {
	Data   string `json:"data,omitempty"`   // the folder that holds originals/ and .stow/: this PC keeps the photos
	Server string `json:"server,omitempty"` // or another PC's tidoly stow, http://<address>:<port>: this PC only looks
	Update string `json:"update,omitempty"` // where releases are downloaded from, over the default — for trying a release
}

func Load(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(b, &c)
	return c, err
}

func (c Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	tmp := path + ".new"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Part is one file fetched on first start. One already in place is not fetched again.
type Part struct {
	Name   string // as the setup page shows it: "ffmpeg (영상의 작은 그림)"
	URL    string
	SHA256 string // of the file as fetched, hex
	Path   string // where it goes; made runnable
}

// State is how far one part has come: "waiting", "fetching", "done", "there" (in place already) or "failed".
type State struct {
	Name  string `json:"name"`
	Got   int64  `json:"got"`
	Size  int64  `json:"size"` // 0 while not known
	State string `json:"state"`
}

// Progress is shared between the fetching and the page that asks how it goes.
type Progress struct {
	mu       sync.Mutex
	parts    []State
	finished bool
	err      string
}

type Report struct {
	Parts    []State `json:"parts"`
	Finished bool    `json:"finished"`
	Error    string  `json:"error,omitempty"`
}

func (p *Progress) Report() Report {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Report{append([]State(nil), p.parts...), p.finished, p.err}
}

func (p *Progress) set(i int, f func(*State)) {
	p.mu.Lock()
	f(&p.parts[i])
	p.mu.Unlock()
}

// Fetch fetches the parts one after another. It stops at the first that fails; run again, it goes on
// from there, the ones done being in place.
func Fetch(ctx context.Context, parts []Part, p *Progress) {
	p.mu.Lock()
	p.parts, p.finished, p.err = make([]State, len(parts)), false, ""
	for i, part := range parts {
		p.parts[i] = State{Name: part.Name, State: "waiting"}
	}
	p.mu.Unlock()
	for i, part := range parts {
		if _, err := os.Stat(part.Path); err == nil {
			p.set(i, func(s *State) { s.State = "there" })
			continue
		}
		p.set(i, func(s *State) { s.State = "fetching" })
		if err := fetch(ctx, part, func(got, size int64) { p.set(i, func(s *State) { s.Got, s.Size = got, size }) }); err != nil {
			p.set(i, func(s *State) { s.State = "failed" })
			p.mu.Lock()
			p.err, p.finished = fmt.Sprintf("%s: %v", part.Name, err), true
			p.mu.Unlock()
			return
		}
		p.set(i, func(s *State) { s.State = "done" })
	}
	p.mu.Lock()
	p.finished = true
	p.mu.Unlock()
}

func fetch(ctx context.Context, part Part, progress func(got, size int64)) error {
	req, err := http.NewRequestWithContext(ctx, "GET", part.URL, nil)
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("%s answered %s", part.URL, res.Status)
	}
	if err := os.MkdirAll(filepath.Dir(part.Path), 0o755); err != nil {
		return err
	}
	tmp := part.Path + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer os.Remove(tmp) // gone once renamed; left behind by nothing
	sum := sha256.New()
	var got int64
	buf := make([]byte, 256<<10)
	for {
		n, err := res.Body.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				out.Close()
				return werr
			}
			sum.Write(buf[:n])
			got += int64(n)
			progress(got, max(res.ContentLength, 0))
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			out.Close()
			return err
		}
	}
	if err := out.Close(); err != nil {
		return err
	}
	if hex.EncodeToString(sum.Sum(nil)) != part.SHA256 {
		return errors.New("the file is not the one expected (checksum)")
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, part.Path)
}

// Invite is what the owner sends a family member: how to join the phone to this server. With an email
// it names the login address; the password is never in it — it is told apart. An https address is one
// that works away from home (the tunnel), so the line about the home Wi-Fi is left out.
func Invite(address, email string, tidoly bool) string {
	b := "tidoly stow 에 사진을 백업하세요.\n\n" +
		"1. 폰에 Immich 앱을 설치합니다 — App Store · Google Play 에서 \"Immich\"\n" +
		"2. 앱을 열고 서버 주소에 넣습니다: " + address + "\n"
	switch {
	case tidoly:
		b += "3. \"Login with tidoly\" 를 누르고, 초대받은 구글 계정으로 로그인합니다. 집 밖에서도 됩니다.\n"
	case email != "":
		b += "3. 로그인 주소: " + email + "\n4. 비밀번호는 따로 알려 드릴게요.\n"
	default:
		b += "3. 로그인 주소와 비밀번호는 따로 알려 드릴게요.\n"
	}
	b += "그다음 앱에서 백업을 켜면 폰의 사진이 저장됩니다.\n"
	if !strings.HasPrefix(address, "https://") {
		b += "\n※ 집 와이파이에 연결된 상태에서 해 주세요."
	}
	return b
}
