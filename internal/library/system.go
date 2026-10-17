package library

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// A folder of photos someone registers (stow issue #15) is read through, every file in it. Registered
// too high — the system disk, the home folder — it would bring in what browsers cache and programs carry:
// icons, web pages' pictures, wallpapers. Two guards: such folders cannot be registered, and inside a good
// folder the program and cache folders are passed over, and so are pictures too small to be photos.

// SystemFolder says whether dir is the top of the system disk, the home folder or above it, or a system
// or program folder or inside one. The top of another disk — a camera's card, an external disk — is fine.
func SystemFolder(dir string) bool {
	dir = filepath.Clean(dir)
	same := func(a, b string) bool {
		if runtime.GOOS == "windows" || runtime.GOOS == "darwin" { // file names ignore case there
			return strings.EqualFold(a, b)
		}
		return a == b
	}
	within := func(a, b string) bool { // a is b or lies under it
		rel, err := filepath.Rel(b, a)
		if err != nil {
			return false
		}
		if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
			rel2, err2 := filepath.Rel(strings.ToLower(b), strings.ToLower(a))
			if err2 == nil {
				rel = rel2
			}
		}
		return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
	}
	home, _ := os.UserHomeDir()
	var top string
	var system []string
	switch runtime.GOOS {
	case "windows":
		top = os.Getenv("SystemDrive") + `\`
		if top == `\` {
			top = `C:\`
		}
		system = []string{os.Getenv("SystemRoot"), os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"),
			os.Getenv("ProgramData"), filepath.Join(home, "AppData")}
	case "darwin":
		top = "/"
		system = []string{"/System", "/Library", "/Applications", "/usr", "/bin", "/sbin", "/opt",
			"/Volumes/Macintosh HD", filepath.Join(home, "Library"), filepath.Join(home, "Applications")}
	default:
		top = "/"
		system = []string{"/usr", "/etc", "/var", "/opt", "/proc", "/sys", "/dev", "/run", "/boot", "/bin", "/sbin",
			"/lib", "/lib64", "/snap", filepath.Join(home, ".cache"), filepath.Join(home, ".local")}
	}
	if same(dir, top) {
		return true
	}
	if home != "" && within(home, dir) { // the home folder, or a folder holding it: /Users, C:\Users
		return true
	}
	for _, s := range system {
		if s != "" && within(dir, s) {
			return true
		}
	}
	return false
}

// PassOver says whether a folder met while reading a registered folder is one to leave out: hidden, the
// recycle bin and the like ($), program and system folders, caches.
func PassOver(name string) bool {
	if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "$") {
		return true
	}
	lower := strings.ToLower(name)
	switch lower {
	case "library", "appdata", "application data", "node_modules", "windows", "program files",
		"program files (x86)", "programdata", "system volume information":
		return true
	}
	return strings.Contains(lower, "cache")
}
