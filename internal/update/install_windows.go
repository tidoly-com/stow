package update

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// install downloads stow-install.exe, checks it against the release's SHA256SUMS, and starts it apart from
// this program: it asks the app to quit, puts the new one in place and opens it.
func install(ctx context.Context, base string) (func() error, error) {
	sums, err := fetch(ctx, base+"/SHA256SUMS")
	if err != nil {
		return nil, err
	}
	want := ""
	for s := bufio.NewScanner(strings.NewReader(string(sums))); s.Scan(); {
		if f := strings.Fields(s.Text()); len(f) == 2 && strings.TrimPrefix(f[1], "*") == "stow-install.exe" {
			want = f[0]
		}
	}
	if want == "" {
		return nil, errors.New("stow-install.exe is not in SHA256SUMS")
	}
	exe, err := fetch(ctx, base+"/stow-install.exe")
	if err != nil {
		return nil, err
	}
	if sum := sha256.Sum256(exe); hex.EncodeToString(sum[:]) != want {
		return nil, errors.New("stow-install.exe does not match its checksum")
	}
	file := filepath.Join(os.TempDir(), "stow-install-update.exe")
	if err := os.WriteFile(file, exe, 0o755); err != nil {
		return nil, err
	}
	cmd := exec.Command(file)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008 | 0x00000200} // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd.Wait, nil
}

func byButton() bool { return true }

func fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, errors.New(url + ": " + res.Status)
	}
	return io.ReadAll(io.LimitReader(res.Body, 200<<20))
}
