// Package library keeps the files: it takes an upload off the wire and puts it where its owner can find it
// without this program — <location>/<person>/<year>/<month>/<the name the phone gave it>. The first
// location is originals/ in the data folder; others are folders the owner of the server adds, on any
// disk (stow issue #14). A file stays in the location it was put in.
package library

import (
	"context"
	"crypto/sha1"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// a root is one location: where its files lie, and where uploads wait until they are whole. The two are
// on the same disk, so that putting a finished upload in its place is a rename and never a copy.
type root struct {
	files, tmp string
	readOnly   bool // someone's own folder: read where it lies, nothing in it made, moved or removed (#15)
}

type Library struct {
	mu    sync.RWMutex
	roots map[string]root // by location id; "" is the data folder
}

const tmpName = ".stow-tmp" // inside an added location

// Hidden is the folder inside the data folder for what this program keeps for itself — the database, the
// small pictures, uploads on their way in — out of sight, so that someone opening the data folder sees
// their photos and nothing to break. Because it lies with the photos, pointing a fresh install at the
// same data folder brings everything back. A data folder from before this folder existed is put in order.
func Hidden(dataDir string) (string, error) {
	dir := filepath.Join(dataDir, ".stow")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	hide(dir)
	for _, name := range []string{"stow.db", "stow.db-wal", "stow.db-shm", "derived"} {
		old, now := filepath.Join(dataDir, name), filepath.Join(dir, name)
		if _, err := os.Lstat(now); err == nil {
			continue
		}
		if _, err := os.Lstat(old); err == nil {
			if err := os.Rename(old, now); err != nil {
				return "", err
			}
		}
	}
	os.RemoveAll(filepath.Join(dataDir, "tmp")) // where uploads used to wait
	return dir, nil
}

// Open prepares the folders under dataDir and throws away uploads a previous run left unfinished.
func Open(dataDir string) (*Library, error) {
	hidden, err := Hidden(dataDir)
	if err != nil {
		return nil, err
	}
	l := &Library{roots: map[string]root{}}
	return l, l.add("", root{files: filepath.Join(dataDir, "originals"), tmp: filepath.Join(hidden, "tmp")})
}

// Add makes the folder at dir a location. It fails if the folder cannot be made or written to; nothing
// already in it is touched.
func (l *Library) Add(id, dir string) error {
	return l.add(id, root{files: dir, tmp: filepath.Join(dir, tmpName)})
}

// AddReadOnly makes the folder at dir a location that is only ever read. It fails if the folder is not
// there; nothing is made in it.
func (l *Library) AddReadOnly(id, dir string) error {
	if info, err := os.Stat(dir); err != nil {
		return err
	} else if !info.IsDir() {
		return &os.PathError{Op: "open", Path: dir, Err: os.ErrInvalid}
	}
	l.mu.Lock()
	l.roots[id] = root{files: dir, readOnly: true}
	l.mu.Unlock()
	return nil
}

// ReadOnly reports whether a location is one that is only read.
func (l *Library) ReadOnly(location string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.roots[location].readOnly
}

// Forget stops using a location in this run.
func (l *Library) Forget(location string) {
	l.mu.Lock()
	delete(l.roots, location)
	l.mu.Unlock()
}

func (l *Library) add(id string, r root) error {
	if err := os.RemoveAll(r.tmp); err != nil {
		return err
	}
	for _, dir := range []string{r.files, r.tmp} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	probe, err := os.CreateTemp(r.tmp, "probe-*") // a folder on a read-only or unplugged disk fails here
	if err != nil {
		return err
	}
	probe.Close()
	os.Remove(probe.Name())
	l.mu.Lock()
	l.roots[id] = r
	l.mu.Unlock()
	return nil
}

// Dir is the folder of a location, "" if this run does not have it (its disk was not there at start).
func (l *Library) Dir(location string) string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.roots[location].files
}

// Received is an upload that is fully on disk but not yet part of the library.
type Received struct {
	tmpPath  string
	Location string
	Checksum []byte
	Size     int64
}

// Receive streams r to a temporary file, hashing as it goes. Nothing is held in memory but one buffer.
// On any error — the phone hung up, the disk filled — the partial file is removed.
func (l *Library) Receive(location string, r io.Reader) (*Received, error) {
	l.mu.RLock()
	to, ok := l.roots[location]
	l.mu.RUnlock()
	if !ok || to.readOnly {
		return nil, &os.PathError{Op: "receive", Path: "location " + location, Err: os.ErrNotExist}
	}
	f, err := os.CreateTemp(to.tmp, "upload-*")
	if err != nil {
		return nil, err
	}
	hash := sha1.New()
	n, err := io.Copy(io.MultiWriter(f, hash), r)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return nil, err
	}
	return &Received{tmpPath: f.Name(), Location: location, Checksum: hash.Sum(nil), Size: n}, nil
}

// Discard removes an upload that will not be kept.
func (r *Received) Discard() { os.Remove(r.tmpPath) }

// Place moves a received file to its place in the location it was received into and returns the path
// relative to that location, with forward slashes. taken reports whether the library already records a
// file at a path there.
func (l *Library) Place(ctx context.Context, r *Received, ownerDir, name string, takenAt time.Time, taken func(ctx context.Context, location, rel string) (bool, error)) (string, error) {
	abs := func(rel string) string { return l.Path(r.Location, rel) }
	name = SafeName(name)
	local := takenAt.Local()
	dir := path.Join(ownerDir, fmt.Sprintf("%04d", local.Year()), fmt.Sprintf("%02d", int(local.Month())))
	if err := os.MkdirAll(abs(dir), 0o755); err != nil {
		return "", err
	}
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; ; i++ {
		rel := path.Join(dir, name)
		if i > 1 {
			rel = path.Join(dir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
		}
		used, err := taken(ctx, r.Location, rel)
		if err != nil {
			return "", err
		}
		if _, statErr := os.Lstat(abs(rel)); used || statErr == nil {
			continue // a different photo already has this name
		}
		// Link fails if the name appeared meanwhile, so two uploads cannot land on the same file
		err = os.Link(r.tmpPath, abs(rel))
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			// no hard links on this file system (exFAT, some network drives): move it instead
			if err := os.Rename(r.tmpPath, abs(rel)); err != nil {
				return "", err
			}
			return rel, nil
		}
		os.Remove(r.tmpPath)
		return rel, nil
	}
}

// Remove deletes a placed file. A file in a read-only location is left alone.
func (l *Library) Remove(location, rel string) {
	if !l.ReadOnly(location) {
		os.Remove(l.Path(location, rel))
	}
}

// Path is where a placed file lies on this machine.
func (l *Library) Path(location, rel string) string {
	dir := l.Dir(location)
	if dir == "" {
		dir = filepath.Join(os.DevNull, "no-such-location") // nothing can be opened or made under it
	}
	return filepath.Join(dir, filepath.FromSlash(rel))
}

// Open opens a placed file for reading.
func (l *Library) Open(location, rel string) (*os.File, error) { return os.Open(l.Path(location, rel)) }

// SafeName reduces what the phone sent to a plain file name that every desktop file system accepts.
func SafeName(name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	name = path.Base(name)
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.TrimRight(name, ". ")
	if name == "" || name == "." || name == ".." {
		return "unnamed"
	}
	return name
}
