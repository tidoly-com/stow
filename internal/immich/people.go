package immich

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/tidoly-com/stow/internal/faces"
	"github.com/tidoly-com/stow/internal/store"
)

// minFaceSide is the smallest face, in pixels of the preview (1440 on its long side), worth keeping.
// Smaller ones are passers-by in the background and toys in someone's hand, and they are too coarse to
// tell apart: on the operator's first photos every face under 50 pixels was one or the other.
const minFaceSide = 60

// repeatLikeness is how alike two faces in one photo must be to be one face put there more than once — a
// collage of the same face, a sticker sheet. On 654 photos with faces, pairs of faces within one photo were
// below 0.6 or well above it: 311 pairs below (different people, and a few alike), 5 between 0.6 and 0.7,
// 204 at 0.7 and over — all in 9 collages (2026-10-18). Treated as people, a collage's repeats became as
// many new persons, since two faces in one photo are kept two people.
const repeatLikeness = 0.6

// withoutRepeats keeps, of faces in one photo that are the same face repeated, the largest.
func withoutRepeats(found []store.Face) []store.Face {
	sorted := append([]store.Face(nil), found...)
	sort.SliceStable(sorted, func(i, j int) bool { return area(sorted[i]) > area(sorted[j]) })
	var kept []store.Face
	for _, f := range sorted {
		repeat := false
		for _, k := range kept {
			if faces.Likeness(f.Embedding, k.Embedding) >= repeatLikeness {
				repeat = true
				break
			}
		}
		if !repeat {
			kept = append(kept, f)
		}
	}
	return kept
}

func area(f store.Face) int { return (f.X2 - f.X1) * (f.Y2 - f.Y1) }

// LookAgainAtRepeats finds the photos read before repeats were left out that hold the same face more than
// once, forgets their faces — a person left with none goes — and looks at them again. It is done at start;
// once they are looked at again there are none.
func (s *Server) LookAgainAtRepeats(ctx context.Context) (photos int, err error) {
	users, err := s.store.Users(ctx)
	if err != nil {
		return 0, err
	}
	for _, u := range users {
		s.faceMu.Lock()
		all, err := s.store.FaceDescriptions(ctx, u.ID)
		s.faceMu.Unlock()
		if err != nil {
			return photos, err
		}
		byPhoto := map[string][]store.Face{}
		for _, f := range all {
			byPhoto[f.AssetID] = append(byPhoto[f.AssetID], f)
		}
		for assetID, list := range byPhoto {
			if len(withoutRepeats(list)) == len(list) {
				continue
			}
			s.faceMu.Lock()
			changed, err := s.store.ForgetFaces(ctx, u.ID, assetID)
			s.faceMu.Unlock()
			if err != nil {
				return photos, err
			}
			for personID, faceID := range changed {
				s.cfg.Thumbs.Remove(personID)
				if faceID != "" {
					if err := s.makePersonPicture(ctx, personID, faceID); err != nil {
						log.Printf("faces: new picture for a person: %v", err)
					}
				}
			}
			if err := s.store.AddJob(ctx, assetID, "face"); err != nil {
				return photos, err
			}
			photos++
		}
		if photos > 0 {
			s.Changed(u.ID)
			s.runner.Wake()
		}
	}
	return photos, nil
}

// FindFaces is the "face" job: ask the helper for the faces in a photo, and put each together with the
// earlier face it most resembles, if it resembles one enough (stow issue #10). It runs on the upright
// preview, so it comes after the thumbnail job. Everything stays on this machine.
func (s *Server) FindFaces(ctx context.Context, assetID string) error {
	asset, err := s.store.AssetByID(ctx, assetID)
	if err != nil {
		return err
	}
	preview := s.cfg.Thumbs.Path(assetID, "preview")
	if _, err := os.Stat(preview); err != nil || asset.Type != "IMAGE" {
		return nil // no picture to look at
	}
	if done, err := s.store.HasFaces(ctx, assetID); err != nil || done {
		return err
	}
	width, height, err := s.cfg.Thumbs.Size(preview)
	if err != nil {
		return err
	}
	helper := s.faces(ctx)
	if helper == nil {
		return errFacesOff // turned off meanwhile: queued afresh when turned on (SetFaces)
	}
	found, err := helper.Detect(ctx, preview, s.lib.Path(asset.Location, asset.Path))
	if err != nil {
		return err
	}
	s.faceMu.Lock() // comparing and recording must not interleave between two photos
	defer s.faceMu.Unlock()
	known, err := s.store.FaceDescriptions(ctx, asset.OwnerID)
	if err != nil {
		return err
	}
	// Two faces in one photo are two people. So the faces of this photo are matched strongest first, and
	// once a person is taken, the other faces of the photo cannot join that person.
	var fresh []store.Face
	for _, f := range found {
		face := store.Face{AssetID: assetID, OwnerID: asset.OwnerID, Score: f.Score, Embedding: f.Embedding, ImageW: width, ImageH: height,
			X1: int(math.Round(f.Box[0] * float64(width))), Y1: int(math.Round(f.Box[1] * float64(height))),
			X2: int(math.Round(f.Box[2] * float64(width))), Y2: int(math.Round(f.Box[3] * float64(height)))}
		if min(face.X2-face.X1, face.Y2-face.Y1) >= minFaceSide && len(f.Embedding) > 0 {
			fresh = append(fresh, face)
		}
	}
	fresh = withoutRepeats(fresh)
	takenPeople, takenFaces := map[string]bool{}, map[string]bool{}
	for len(fresh) > 0 {
		pick, like, best := 0, "", s.cfg.FaceThreshold
		for i, face := range fresh {
			for _, k := range known {
				if takenFaces[k.ID] || k.PersonID != "" && takenPeople[k.PersonID] {
					continue
				}
				if v := faces.Likeness(face.Embedding, k.Embedding); v > best {
					pick, like, best = i, k.ID, v
				}
			}
		}
		face := fresh[pick]
		fresh = append(fresh[:pick], fresh[pick+1:]...)
		created, err := s.store.AddFace(ctx, &face, like)
		if err != nil {
			return err
		}
		if created != "" {
			if err := s.makePersonPicture(ctx, created, like); err != nil {
				log.Printf("faces: picture for a new person: %v", err)
			}
		}
		if like != "" {
			takenFaces[like], takenPeople[face.PersonID] = true, true
		}
		takenFaces[face.ID] = true // a face of this photo is no match for the photo's other faces
		known = append(known, store.Face{ID: face.ID, PersonID: face.PersonID, Embedding: face.Embedding})
	}
	s.hub.emit(asset.OwnerID, "on_asset_update", map[string]any{"id": assetID}) // the app answers with a sync, which brings people too
	return nil
}

// makePersonPicture cuts the face that stands for a person out of its photo's preview.
func (s *Server) makePersonPicture(ctx context.Context, personID, faceID string) error {
	face, err := s.store.FaceByID(ctx, faceID)
	if err != nil {
		return err
	}
	return s.cfg.Thumbs.Crop(s.cfg.Thumbs.Path(face.AssetID, "preview"), s.cfg.Thumbs.Path(personID, "person"),
		face.X1, face.Y1, face.X2, face.Y2)
}

// QueueMissingFaces queues the face job for photos that were never looked at.
func (s *Server) QueueMissingFaces(ctx context.Context) (queued int, err error) {
	if s.faces(ctx) == nil { // off, or the models not here yet: EnableFaces and SetFaces queue them later
		return 0, nil
	}
	ids, err := s.store.AssetsWithoutJob(ctx, "face", true)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err := os.Stat(s.cfg.Thumbs.Path(id, "preview")); err != nil {
			continue // the thumbnail job will queue it when the preview exists
		}
		if err := s.store.AddJob(ctx, id, "face"); err != nil {
			return queued, err
		}
		queued++
	}
	return queued, nil
}

func (s *Server) servePersonPicture(w http.ResponseWriter, r *http.Request, sess session) {
	person, err := s.store.Person(r.Context(), sess.user.ID, r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, 404, "Not found")
		return
	}
	if err != nil {
		writeError(w, 500, "Internal error")
		return
	}
	f, err := os.Open(s.cfg.Thumbs.Path(person.ID, "person"))
	if err != nil {
		writeError(w, 404, "Not found")
		return
	}
	defer f.Close()
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeContent(w, r, person.ID+".jpg", time.Time{}, f)
}

// updatePerson is PUT /api/people/{id}: the app naming a person, or hiding one.
func (s *Server) updatePerson(w http.ResponseWriter, r *http.Request, sess session) {
	var req struct {
		Name     *string
		IsHidden *bool
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req) != nil || req.Name == nil && req.IsHidden == nil {
		writeError(w, 400, "This server keeps a person's name and whether they are hidden")
		return
	}
	id, err := r.PathValue("id"), error(nil)
	if req.Name != nil {
		err = s.store.NamePerson(r.Context(), sess.user.ID, id, strings.TrimSpace(*req.Name))
	}
	if err == nil && req.IsHidden != nil {
		err = s.store.HidePerson(r.Context(), sess.user.ID, id, *req.IsHidden)
	}
	var p store.Person
	if err == nil {
		p, err = s.store.Person(r.Context(), sess.user.ID, id)
	}
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, 404, "Not found")
		return
	}
	if err != nil {
		log.Printf("people: %v", err)
		writeError(w, 500, "Internal error")
		return
	}
	s.Changed(sess.user.ID)
	writeJSON(w, 200, map[string]any{"id": p.ID, "name": p.Name, "birthDate": nil, "isHidden": p.Hidden, "isFavorite": false,
		"thumbnailPath": "/people/" + p.ID + "/thumbnail", "updatedAt": p.UpdatedAt, "color": nil})
}

// personData is SyncPersonV1.
func personData(p store.Person) map[string]any {
	return map[string]any{"id": p.ID, "ownerId": p.OwnerID, "name": p.Name, "isHidden": p.Hidden, "isFavorite": false,
		"birthDate": nil, "color": nil, "faceAssetId": p.FaceID, "createdAt": p.CreatedAt, "updatedAt": p.UpdatedAt}
}

// faceData is SyncAssetFaceV3.
func faceData(f store.Face) map[string]any {
	var person any
	if f.PersonID != "" {
		person = f.PersonID
	}
	return map[string]any{"id": f.ID, "assetId": f.AssetID, "personId": person, "imageWidth": f.ImageW, "imageHeight": f.ImageH,
		"boundingBoxX1": f.X1, "boundingBoxY1": f.Y1, "boundingBoxX2": f.X2, "boundingBoxY2": f.Y2,
		"sourceType": "machine-learning", "isVisible": true, "deletedAt": nil}
}

var errFacesOff = errors.New("face grouping is off")

// faces is the face helper, when grouping faces is on and the models are there; nil otherwise.
func (s *Server) faces(ctx context.Context) *faces.Helper {
	h := s.faceHelper.Load()
	if h == nil || !s.store.FacesOn(ctx) {
		return nil
	}
	return h
}

// FacesState says how face grouping stands, for the pages: "none" — not on this PC (no helper for it);
// "waiting" — the models are being fetched; "on"; "off" — turned off in the admin page.
func (s *Server) FacesState(ctx context.Context) string {
	switch {
	case s.faceHelper.Load() == nil && s.faceWaiting.Load():
		return "waiting"
	case s.faceHelper.Load() == nil:
		return "none"
	case !s.store.FacesOn(ctx):
		return "off"
	}
	return "on"
}

// WaitForFaces says the models are on their way; EnableFaces follows when they are there.
func (s *Server) WaitForFaces() { s.faceWaiting.Store(true) }

// EnableFaces starts face grouping with a helper whose models are there: the photos not looked at yet
// are queued.
func (s *Server) EnableFaces(ctx context.Context, h *faces.Helper) error {
	s.faceHelper.Store(h)
	s.faceWaiting.Store(false)
	if !s.store.FacesOn(ctx) {
		return nil
	}
	_, err := s.QueueMissingFaces(ctx)
	return err
}

// SetFaces switches face grouping on or off. Off, what waits is dropped and the helper ends — its memory
// goes back; the people found stay. On, every photo not looked at yet is queued again.
func (s *Server) SetFaces(ctx context.Context, on bool) error {
	if err := s.store.SetFacesOn(ctx, on); err != nil {
		return err
	}
	if !on {
		if h := s.faceHelper.Load(); h != nil {
			h.Close()
		}
		return s.store.ForgetJobs(ctx, "face", "queued")
	}
	if err := s.store.ForgetJobs(ctx, "face", "queued", "failed"); err != nil {
		return err
	}
	_, err := s.QueueMissingFaces(ctx)
	s.runner.Wake()
	return err
}
