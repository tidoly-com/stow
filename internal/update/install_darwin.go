package update

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// install runs the Mac install script the customer ran, apart from this program: it quits the app — this
// server with it — replaces it and opens it again. Its words go to the logs folder.
func install(ctx context.Context, base string) (*exec.Cmd, error) {
	logs := filepath.Join(os.Getenv("HOME"), "Library", "Logs", "tidoly-stow")
	os.MkdirAll(logs, 0o755)
	out, err := os.OpenFile(filepath.Join(logs, "update.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	cmd := exec.Command("/bin/sh", "-c", `curl -fsSL "$1/stow-macos.sh" | STOW_BASE_URL="$1" /bin/sh`, "sh", base)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // lives on when the app it replaces quits
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}
