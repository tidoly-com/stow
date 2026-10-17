package immich

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"errors"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/tidoly-com/stow/internal/disk"
	"github.com/tidoly-com/stow/internal/store"
)

// LoadLocations opens the storage locations the database knows (stow issue #14). One whose disk is not
// there is left out for this run: its photos cannot be shown and nothing new goes to it.
func (s *Server) LoadLocations(ctx context.Context) error {
	found, err := s.store.Locations(ctx)
	if err != nil {
		return err
	}
	for _, l := range found {
		if l.ID == "" {
			continue
		}
		add := s.lib.Add
		if l.Kind == store.KindFolder {
			add = s.lib.AddReadOnly
		}
		if err := add(l.ID, l.Path); err != nil {
			log.Printf("storage: %s cannot be used now: %v", l.Path, err)
		}
	}
	return nil
}

// LocationDir is the folder of a location as this run uses it, "" if its disk is not there.
func (s *Server) LocationDir(id string) string { return s.lib.Dir(id) }

// AddLocation makes a folder a place for photos. The folder is made if it is not there, and must be
// writable; nothing already in it is touched.
func (s *Server) AddLocation(ctx context.Context, dir string) (string, error) {
	if !filepath.IsAbs(dir) {
		return "", errors.New("not a full path")
	}
	dir = filepath.Clean(dir)
	if err := s.apart(ctx, dir); err != nil {
		return "", err
	}
	if err := s.onAnotherDisk(ctx, dir); err != nil {
		return "", err
	}
	id, err := s.store.AddLocation(ctx, dir, store.KindStore, "")
	if err != nil {
		return "", err
	}
	if err := s.lib.Add(id, dir); err != nil {
		s.store.RemoveLocation(ctx, id)
		return "", err
	}
	return id, nil
}

// ErrSameDisk is a place on a disk photos are kept on already: adding it would bring no room.
type ErrSameDisk struct{ Place string }

func (e ErrSameDisk) Error() string { return "on the same disk as " + e.Place }

// sameDisk tells whether two folders are on one disk; tests, whose folders all are, put it aside.
var sameDisk = disk.Same

// onAnotherDisk makes sure a new place for photos is on a disk none of the places in use is on: a place
// is added for room, and another folder on the same disk gives none (operator, 2026-10-15).
func (s *Server) onAnotherDisk(ctx context.Context, dir string) error {
	found, err := s.store.Locations(ctx)
	if err != nil {
		return err
	}
	for _, l := range found {
		if l.Kind == store.KindFolder {
			continue // someone's own photos read where they lie: not a place photos are put
		}
		place := l.Path
		if l.ID == "" {
			place = filepath.Dir(s.lib.Dir(""))
		}
		if same, err := sameDisk(dir, place); err == nil && same {
			return ErrSameDisk{Place: place}
		}
	}
	return nil
}

// apart makes sure a folder neither holds nor lies inside a folder already in use, so that no file can
// be reached by two ways.
func (s *Server) apart(ctx context.Context, dir string) error {
	found, err := s.store.Locations(ctx)
	if err != nil {
		return err
	}
	inside := func(a, b string) bool { // a is b or lies under it
		rel, err := filepath.Rel(b, a)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	for _, l := range found {
		other := l.Path
		if l.ID == "" {
			other = filepath.Dir(s.lib.Dir("")) // the data folder itself
		}
		if other != dir && (inside(dir, other) || inside(other, dir)) {
			return errors.New("overlaps " + other)
		}
	}
	return nil
}

// ── folders someone already had (stow issue #15) ────────────────────────────────────────────────────────────

// Scan is how the latest look at a read-only folder went, or is going.
type Scan struct {
	Running                 atomic.Bool
	Seen, Added, Same, Gone atomic.Int64 // files looked at, new to the library, already in it from elsewhere, no longer there
	Moved                   atomic.Int64 // found under another path or name than before
	Failure                 atomic.Value // string: why the look stopped early
}

// AddFolder takes a folder of photos someone already has into ownerID's library, where it lies: the
// files are read, recorded and given small pictures, and never moved, changed or deleted. The first look
// at it starts at once, in the background.
func (s *Server) AddFolder(ctx context.Context, dir, ownerID string) (string, error) {
	if !filepath.IsAbs(dir) {
		return "", errors.New("not a full path")
	}
	dir = filepath.Clean(dir)
	if systemFolder(dir) {
		return "", ErrSystemFolder{Folder: dir}
	}
	if err := s.apart(ctx, dir); err != nil {
		return "", err
	}
	id, err := s.store.AddLocation(ctx, dir, store.KindFolder, ownerID)
	if err != nil {
		return "", err
	}
	if err := s.lib.AddReadOnly(id, dir); err != nil {
		s.store.RemoveLocation(ctx, id)
		return "", err
	}
	s.ScanFolder(id)
	return id, nil
}

// ScanFolder looks at a read-only folder again, in the background, unless a look is already going on.
func (s *Server) ScanFolder(id string) {
	fresh := &Scan{}
	fresh.Running.Store(true)
	if old, ok := s.scans.Load(id); ok && old.(*Scan).Running.Load() {
		return
	}
	s.scans.Store(id, fresh)
	go func() {
		defer fresh.Running.Store(false)
		if err := s.scan(context.Background(), id, fresh); err != nil {
			log.Printf("folder %s: %v", id, err)
			fresh.Failure.Store(err.Error())
		}
	}()
}

// ScanFolders looks at every read-only folder whose disk is there. Done once at start, it picks up where
// a look that was cut short left off, and what changed in the folders while the program was not running.
// A file that has not changed costs one look at its size and time, not a reading of it.
func (s *Server) ScanFolders(ctx context.Context) {
	found, err := s.store.Locations(ctx)
	if err != nil {
		log.Printf("folders: %v", err)
		return
	}
	for _, l := range found {
		if l.Kind == store.KindFolder && !l.Paused && s.lib.Dir(l.ID) != "" {
			s.ScanFolder(l.ID)
		}
	}
}

// PauseFolder switches a read-only folder off for now, or on again. Off, its photos leave the library
// and nothing is looked at; what was read of them is kept. On, they are back at once, and the folder is
// looked at for what changed meanwhile.
func (s *Server) PauseFolder(ctx context.Context, id string, paused bool) error {
	loc, err := s.store.Location(ctx, id)
	if err != nil || loc.Kind != store.KindFolder {
		return errors.New("not a folder that is only read")
	}
	if scan := s.FolderScan(id); scan != nil && scan.Running.Load() {
		return errors.New("still being looked at")
	}
	if err := s.store.PauseLocation(ctx, id, paused); err != nil {
		return err
	}
	s.Changed(loc.OwnerID)
	if !paused && s.lib.Dir(id) != "" {
		s.ScanFolder(id)
	}
	return nil
}

// FolderScan reports the latest look at a folder; nil if there was none in this run.
func (s *Server) FolderScan(id string) *Scan {
	if found, ok := s.scans.Load(id); ok {
		return found.(*Scan)
	}
	return nil
}

var imageExt = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".heic": true, ".heif": true, ".webp": true, ".gif": true}

// scan brings the library in line with what is in the folder now: new files come in, files that are
// gone leave, files their owner deleted from the library stay out. Nothing in the folder is touched.
func (s *Server) scan(ctx context.Context, id string, report *Scan) error {
	loc, err := s.store.Location(ctx, id)
	if err != nil {
		return err
	}
	if loc.Paused {
		return nil // switched off: left as it is
	}
	if _, err := os.Stat(loc.Path); err != nil {
		return err // the disk is not there: forget nothing
	}
	known, err := s.store.FilesAt(ctx, id)
	if err != nil {
		return err
	}
	skipped, err := s.store.Skipped(ctx, id)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	err = filepath.WalkDir(loc.Path, func(full string, entry fs.DirEntry, err error) error {
		if err != nil {
			if full == loc.Path {
				return err
			}
			return nil // a folder inside that cannot be read is passed over
		}
		name := entry.Name()
		if entry.IsDir() {
			if full != loc.Path && passOver(name) {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(name))
		if strings.HasPrefix(name, ".") || !(imageExt[ext] || videoExt[ext]) || !entry.Type().IsRegular() {
			return nil
		}
		relative, _ := filepath.Rel(loc.Path, full)
		rel := filepath.ToSlash(relative)
		seen[rel] = true
		report.Seen.Add(1)
		info, err := entry.Info()
		if err != nil || skipped[rel] || (imageExt[ext] && info.Size() < tooSmall) {
			return nil
		}
		modified := store.Timestamp(info.ModTime())
		if was, ok := known[rel]; ok {
			if was.Size == info.Size() && was.Modified == modified {
				return nil // recorded, and not changed since
			}
			s.deleteAsset(ctx, was.OwnerID, was.AssetID, false) // another file under the same name now
		}
		sum, err := checksum(full)
		if err != nil {
			return nil
		}
		asset := store.Asset{ID: store.NewID(), OwnerID: loc.OwnerID, Checksum: sum, OriginalName: name, Location: id, Path: rel,
			Size: info.Size(), Type: "IMAGE", FileCreatedAt: modified, FileModifiedAt: modified}
		if videoExt[ext] {
			asset.Type = "VIDEO"
		}
		switch err := s.store.AddAsset(ctx, &asset, s.cfg.JobKinds); {
		case errors.Is(err, store.ErrDuplicate):
			// The same photo is in the library already. If it is recorded in this folder under a path where
			// no file is any more, the file was moved or renamed: the record follows it. Otherwise the
			// photo came from the phone or another folder, and this copy is left out.
			if was, err := s.store.AssetByChecksum(ctx, loc.OwnerID, sum); err == nil && was.Location == id && was.Path != rel && !seen[was.Path] {
				if _, gone := os.Stat(filepath.Join(loc.Path, filepath.FromSlash(was.Path))); gone != nil {
					if err := s.store.MoveFile(ctx, was.ID, rel, name, modified); err != nil {
						return err
					}
					delete(known, was.Path)
					report.Moved.Add(1)
					break
				}
			}
			report.Same.Add(1)
		case err != nil:
			return err
		default:
			if report.Added.Add(1)%50 == 1 {
				s.runner.Wake()
			}
		}
		return ctx.Err()
	})
	if err != nil {
		return err
	}
	for rel, was := range known {
		if !seen[rel] {
			if err := s.deleteAsset(ctx, was.OwnerID, was.AssetID, false); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			report.Gone.Add(1)
		}
	}
	s.runner.Wake()
	if report.Added.Load()+report.Gone.Load()+report.Moved.Load() > 0 {
		s.Changed(loc.OwnerID)
	}
	return nil
}

func checksum(file string) ([]byte, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	hash := sha1.New()
	if _, err := io.Copy(hash, f); err != nil {
		return nil, err
	}
	return hash.Sum(nil), nil
}

// RemoveFolder takes a read-only folder out of the library again: its photos are forgotten, its files
// stay as they are.
func (s *Server) RemoveFolder(ctx context.Context, id string) error {
	loc, err := s.store.Location(ctx, id)
	if err != nil || loc.Kind != store.KindFolder {
		return errors.New("not a folder that is only read")
	}
	if scan := s.FolderScan(id); scan != nil && scan.Running.Load() {
		return errors.New("still being looked at")
	}
	known, err := s.store.FilesAt(ctx, id)
	if err != nil {
		return err
	}
	for _, was := range known {
		if err := s.deleteAsset(ctx, was.OwnerID, was.AssetID, false); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	s.lib.Forget(id)
	s.Changed(loc.OwnerID)
	return s.store.RemoveLocation(ctx, id)
}
