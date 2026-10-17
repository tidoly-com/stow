package immich

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tidoly-com/stow/internal/library"
	"github.com/tidoly-com/stow/internal/media"
	"github.com/tidoly-com/stow/internal/places"
	"github.com/tidoly-com/stow/internal/store"
	"github.com/tidoly-com/stow/internal/thumbs"
)

var videoExt = map[string]bool{".mp4": true, ".mov": true, ".m4v": true, ".3gp": true, ".webm": true, ".mkv": true, ".avi": true}

// 1x1 JPEG, standing in for a video's still until thumbnails exist (stow issue #7)
var tinyJPEG, _ = base64.StdEncoding.DecodeString(
	"/9j/4AAQSkZJRgABAQEASABIAAD/2wBDAP//////////////////////////////////////////////////////////////////////////////////////" +
		"wgALCAABAAEBAREA/8QAFBABAAAAAAAAAAAAAAAAAAAAAP/aAAgBAQABPxA=")

// upload takes one photo or video: POST /api/assets, a multipart form with the file in "assetData".
// The file goes straight to disk; only the small text fields are held in memory.
func (s *Server) upload(w http.ResponseWriter, r *http.Request, sess session) {
	ctx := r.Context()
	parts, err := r.MultipartReader()
	if err != nil {
		writeError(w, 400, "Expected a multipart form")
		return
	}
	location, err := s.store.LocationFor(ctx, sess.user.ID) // the disk this person's photos go to now
	if err != nil {
		log.Printf("upload: storage location: %v", err)
		writeError(w, 500, "Internal error")
		return
	}
	fields := map[string]string{}
	var received *library.Received
	var partName string
	for {
		part, err := parts.NextPart()
		if err == io.EOF {
			break
		}
		if err == nil && part.FormName() == "assetData" && received == nil {
			partName = part.FileName()
			received, err = s.lib.Receive(location, part)
		} else if err == nil {
			var value []byte
			value, err = io.ReadAll(io.LimitReader(part, 64<<10))
			fields[part.FormName()] = string(value)
		}
		if err != nil {
			if received != nil {
				received.Discard()
			}
			var pathErr *fs.PathError
			switch {
			case errors.Is(err, syscall.ENOSPC):
				log.Printf("upload: disk is full")
				writeError(w, http.StatusInsufficientStorage, "The disk is full")
			case errors.As(err, &pathErr):
				log.Printf("upload: %v", err)
				writeError(w, 500, "Could not store the file")
			default: // the phone hung up or sent a broken form
				writeError(w, 400, "Upload was cut short")
			}
			return
		}
	}
	if received == nil {
		writeError(w, 400, "No assetData in the form")
		return
	}
	// the whole file is here: finish recording it even if the phone hangs up now
	ctx = context.WithoutCancel(ctx)

	if existing, err := s.store.AssetByChecksum(ctx, sess.user.ID, received.Checksum); err == nil {
		received.Discard()
		if existing.DeletedAt != "" { // uploading a photo that is in the trash takes it back out
			s.Restore(ctx, sess.user.ID, []string{existing.ID})
		}
		writeJSON(w, 200, map[string]any{"id": existing.ID, "status": "duplicate"})
		return
	}
	name := fields["filename"]
	if name == "" {
		name = partName
	}
	name = library.SafeName(name)
	created := parseTime(fields["fileCreatedAt"], time.Now())
	modified := parseTime(fields["fileModifiedAt"], created)
	rel, err := s.lib.Place(ctx, received, sess.user.Dir, name, created, s.store.PathTaken)
	if err != nil {
		received.Discard()
		log.Printf("upload: place: %v", err)
		writeError(w, 500, "Could not store the file")
		return
	}
	asset := store.Asset{ID: store.NewID(), OwnerID: sess.user.ID, Checksum: received.Checksum, OriginalName: name, Location: received.Location, Path: rel,
		Size: received.Size, Type: "IMAGE", FileCreatedAt: store.Timestamp(created), FileModifiedAt: store.Timestamp(modified)}
	if videoExt[strings.ToLower(path.Ext(name))] {
		asset.Type = "VIDEO"
	}
	asset.DurationMs, _ = strconv.ParseInt(fields["duration"], 10, 64)
	err = s.store.AddAsset(ctx, &asset, s.cfg.JobKinds)
	if errors.Is(err, store.ErrDuplicate) { // the same file arrived twice at once and the other one won
		s.lib.Remove(received.Location, rel)
		if existing, err := s.store.AssetByChecksum(ctx, sess.user.ID, received.Checksum); err == nil {
			writeJSON(w, 200, map[string]any{"id": existing.ID, "status": "duplicate"})
			return
		}
	}
	if err != nil {
		s.lib.Remove(received.Location, rel)
		log.Printf("upload: record: %v", err)
		writeError(w, 500, "Could not record the file")
		return
	}
	if video := fields["livePhotoVideoId"]; video != "" { // the app sends the moving half first and names it here
		s.store.SetLive(ctx, sess.user.ID, video, asset.ID)
	}
	s.runner.Wake() // the thumbnail job tells the person's devices when the photo is ready to show
	writeJSON(w, 201, map[string]any{"id": asset.ID, "status": "created"})
}

// updateAssets is PUT /api/assets, the app's way of changing several photos at once. Of what it can
// carry, this server keeps the favourite mark; a request that asks only for something else is refused,
// so that the app does not show a change that was not made.
func (s *Server) updateAssets(w http.ResponseWriter, r *http.Request, sess session) {
	var req struct {
		IDs        []string `json:"ids"`
		IsFavorite *bool    `json:"isFavorite"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req) != nil {
		writeError(w, 400, "Bad request")
		return
	}
	if req.IsFavorite == nil {
		writeError(w, 400, "This server keeps only the favourite mark")
		return
	}
	n, err := s.store.SetFavorite(r.Context(), sess.user.ID, req.IDs, *req.IsFavorite)
	if err != nil {
		log.Printf("favourite: %v", err)
		writeError(w, 500, "Internal error")
		return
	}
	if n > 0 {
		s.hub.emit(sess.user.ID, "on_asset_update", map[string]any{})
	}
	w.WriteHeader(http.StatusNoContent)
}

// TrashDays is how long a photo stays in the trash before it is deleted for good.
const TrashDays = 30

// deleteAssets is DELETE /api/assets. What the app calls "delete" moves photos to the trash: they vanish
// from the timeline on every device, and the files stay on disk until the trash is emptied or TrashDays
// have passed. Only "force" — the app's "delete permanently" — removes them at once (stow issue #17).
func (s *Server) deleteAssets(w http.ResponseWriter, r *http.Request, sess session) {
	var req struct {
		IDs   []string `json:"ids"`
		Force bool     `json:"force"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req) != nil {
		writeError(w, 400, "Bad request")
		return
	}
	if !req.Force {
		if err := s.Trash(r.Context(), sess.user.ID, req.IDs); err != nil {
			log.Printf("trash: %v", err)
			writeError(w, 500, "Could not delete")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	for _, id := range req.IDs {
		if err := s.DeleteAsset(r.Context(), sess.user.ID, id); err != nil && !errors.Is(err, sql.ErrNoRows) {
			log.Printf("delete: %v", err)
			writeError(w, 500, "Could not delete")
			return
		} // someone else's photo, or one already gone, is passed over without a word
	}
	w.WriteHeader(http.StatusNoContent)
}

// Trash moves a person's photos to the trash and tells their devices. Someone else's photos are passed over.
func (s *Server) Trash(ctx context.Context, ownerID string, ids []string) error {
	if parts, err := s.store.LiveParts(ctx, ownerID, ids); err == nil {
		ids = append(ids[:len(ids):len(ids)], parts...) // the moving half of a live photo goes where the photo goes
	}
	if _, err := s.store.SetTrashed(ctx, ownerID, ids, true); err != nil {
		return err
	}
	s.hub.emit(ownerID, "on_asset_trash", ids)
	return nil
}

// trashRestore brings photos back: the ones named, or all of them.
func (s *Server) trashRestore(all bool) func(http.ResponseWriter, *http.Request, session) {
	return func(w http.ResponseWriter, r *http.Request, sess session) {
		var req struct {
			IDs []string `json:"ids"`
		}
		if !all && (json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req) != nil || req.IDs == nil) {
			writeError(w, 400, "Bad request")
			return
		}
		n, err := s.Restore(r.Context(), sess.user.ID, req.IDs)
		if err != nil {
			log.Printf("trash: %v", err)
			writeError(w, 500, "Could not restore")
			return
		}
		writeJSON(w, 200, map[string]any{"count": n})
	}
}

// Restore brings a person's photos back out of the trash; nil ids means all of them.
func (s *Server) Restore(ctx context.Context, ownerID string, ids []string) (int, error) {
	if parts, err := s.store.LiveParts(ctx, ownerID, ids); err == nil && len(ids) > 0 {
		ids = append(ids[:len(ids):len(ids)], parts...)
	}
	n, err := s.store.SetTrashed(ctx, ownerID, ids, false)
	if err == nil && n > 0 {
		s.hub.emit(ownerID, "on_asset_restore", ids)
	}
	return n, err
}

func (s *Server) trashEmpty(w http.ResponseWriter, r *http.Request, sess session) {
	n, err := s.EmptyTrash(r.Context(), sess.user.ID)
	if err != nil {
		log.Printf("trash: %v", err)
		writeError(w, 500, "Could not empty the trash")
		return
	}
	writeJSON(w, 200, map[string]any{"count": n})
}

// EmptyTrash deletes for good everything a person has in the trash.
func (s *Server) EmptyTrash(ctx context.Context, ownerID string) (int, error) {
	ids, err := s.store.Trashed(ctx, ownerID)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := s.DeleteAsset(ctx, ownerID, id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// PurgeTrash deletes for good what has been in the trash longer than TrashDays, now and once a day.
func (s *Server) PurgeTrash(ctx context.Context) {
	tick := time.NewTicker(time.Hour) // a photo goes within the hour its time is up; the look is a small query
	defer tick.Stop()
	for {
		owners, ids, err := s.store.TrashedBefore(ctx, time.Now().AddDate(0, 0, -TrashDays))
		if err != nil {
			log.Printf("trash: %v", err)
		}
		for i := range ids {
			if err := s.DeleteAsset(ctx, owners[i], ids[i]); err != nil {
				log.Printf("trash: %v", err)
			}
		}
		if len(ids) > 0 {
			log.Printf("trash: %d photos deleted for good after %d days", len(ids), TrashDays)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// DeleteAsset removes one photo of a person's entirely and tells their devices.
func (s *Server) DeleteAsset(ctx context.Context, ownerID, assetID string) error {
	return s.deleteAsset(ctx, ownerID, assetID, true)
}

// deleteAsset removes one photo from the library. Its file is deleted too — unless it lies in a folder
// that is only read (#15): that file stays, and with skip it is noted so the next look at the folder
// does not bring it back. Without skip the photo is merely forgotten, as when its file is gone.
func (s *Server) deleteAsset(ctx context.Context, ownerID, assetID string, skip bool) error {
	if parts, err := s.store.LiveParts(ctx, ownerID, []string{assetID}); err == nil {
		for _, part := range parts { // the moving half of a live photo goes with it
			s.deleteAsset(ctx, ownerID, part, skip)
		}
	}
	s.faceMu.Lock() // not while a new face is being compared against this photo's faces
	removed, err := s.store.DeleteAsset(ctx, ownerID, assetID)
	s.faceMu.Unlock()
	if err != nil {
		return err
	}
	if skip && s.lib.ReadOnly(removed.Location) {
		s.store.Skip(ctx, removed.Location, removed.Path)
	}
	s.lib.Remove(removed.Location, removed.Path) // leaves a read-only folder's file alone
	s.cfg.Thumbs.Remove(assetID)
	for personID, faceID := range removed.People {
		s.cfg.Thumbs.Remove(personID)
		if faceID != "" {
			if err := s.makePersonPicture(ctx, personID, faceID); err != nil {
				log.Printf("delete: new picture for a person: %v", err)
			}
		}
	}
	s.hub.emit(ownerID, "on_asset_delete", assetID)
	return nil
}

func parseTime(value string, fallback time.Time) time.Time {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02T15:04Z07:00"} {
		if t, err := time.Parse(layout, value); err == nil {
			return t
		}
	}
	return fallback
}

func (s *Server) serveOriginal(w http.ResponseWriter, r *http.Request, sess session) {
	asset, err := s.store.AssetFor(r.Context(), sess.user.ID, r.PathValue("id")) // one's own, or one that is shared
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, 404, "Not found") // also the answer for someone else's photo
		return
	}
	if err != nil {
		writeError(w, 500, "Internal error")
		return
	}
	f, err := s.lib.Open(asset.Location, asset.Path)
	if err != nil {
		log.Printf("serve: %s is recorded but missing on disk: %v", asset.Path, err)
		writeError(w, 404, "Not found")
		return
	}
	defer f.Close()
	modified, _ := time.Parse(time.RFC3339Nano, asset.FileModifiedAt)
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "private, max-age=86400")
	}
	http.ServeContent(w, r, asset.OriginalName, modified, f) // handles ranges, which video players ask for
}

// serveThumbnail sends a small picture made earlier by the thumbnail job. It never makes one: a request
// that arrives before the job has run gets the original (a photo) or a blank (a video) instead.
func (s *Server) serveThumbnail(w http.ResponseWriter, r *http.Request, sess session) {
	asset, err := s.store.AssetFor(r.Context(), sess.user.ID, r.PathValue("id")) // one's own, or one that is shared
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, 404, "Not found")
		return
	}
	if err != nil {
		writeError(w, 500, "Internal error")
		return
	}
	sizes := []string{"thumbnail"}
	if size := r.URL.Query().Get("size"); size == "preview" || size == "fullsize" {
		sizes = []string{"preview", "thumbnail"}
	}
	for _, size := range sizes {
		if f, err := os.Open(s.cfg.Thumbs.Path(asset.ID, size)); err == nil {
			defer f.Close()
			stat, _ := f.Stat()
			w.Header().Set("Cache-Control", "private, max-age=86400")
			http.ServeContent(w, r, asset.ID+".jpg", stat.ModTime(), f)
			return
		}
	}
	if asset.Type == "VIDEO" {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "no-store") // the real one is on its way
		w.Write(tinyJPEG)
		return
	}
	w.Header().Set("Cache-Control", "no-store") // stands in for the small picture until it is made; a browser that cannot show it (HEIC) must ask again
	s.serveOriginal(w, r, sess)
}

// MakeThumbnail is the "thumbnail" job: make the small pictures, then tell the person's devices that the
// photo is in the library and ready to show (stow issue #7). The devices are told even if no picture
// could be made — the photo is stored either way.
func (s *Server) MakeThumbnail(ctx context.Context, assetID string) error {
	asset, err := s.store.AssetByID(ctx, assetID)
	if err != nil {
		return err
	}
	defer func() {
		if ctx.Err() != nil {
			return
		}
		if fresh, err := s.store.AssetByID(ctx, assetID); err == nil {
			s.hub.emit(fresh.OwnerID, "AssetUploadReadyV2", map[string]any{"asset": assetData(fresh), "exif": exifData(fresh)})
		}
	}()
	path := s.lib.Path(asset.Location, asset.Path)
	info, err := media.Read(path, time.Local)
	if err != nil {
		return err
	}
	err = s.cfg.Thumbs.Make(ctx, path, assetID, info)
	if errors.Is(err, thumbs.ErrNoDecoder) {
		log.Printf("thumbnail: %s needs ffmpeg, which was not found — it will have no small picture", asset.OriginalName)
		return nil
	}
	if err == nil && s.faces(ctx) != nil && asset.Type == "IMAGE" && info.Kind != "" {
		if err := s.store.AddJob(ctx, assetID, "face"); err != nil {
			return err
		}
		s.runner.Wake()
	}
	return err
}

// QueueUndatedEdits reads again the iPhone edited copies read without a capture time — before copies took
// the original's — so that they leave the day they were exported.
func (s *Server) QueueUndatedEdits(ctx context.Context) (queued int, err error) {
	ids, err := s.store.UndatedEdits(ctx)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := s.store.AddJob(ctx, id, "metadata"); err != nil {
			return queued, err
		}
		queued++
	}
	if queued > 0 {
		s.runner.Wake()
	}
	return queued, nil
}

// QueueMissingThumbnails queues the thumbnail job for every asset without small pictures: photos taken
// in by a version of the program that made none, or a derived/ folder that was deleted.
func (s *Server) QueueMissingThumbnails(ctx context.Context) (queued int, err error) {
	ids, err := s.store.AssetsWithoutJob(ctx, "thumbnail", false)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if s.cfg.Thumbs.Has(id) {
			continue
		}
		if err := s.store.AddJob(ctx, id, "thumbnail"); err != nil {
			return queued, err
		}
		queued++
	}
	return queued, nil
}

// ── sync ────────────────────────────────────────────────────────────────────────────────────────────────────

// syncStream sends what changed since this session last confirmed, one JSON object per line. Each line
// carries an ack of the form "<type>|<sequence>"; the app posts those back to /sync/ack as it stores them.
func (s *Server) syncStream(w http.ResponseWriter, r *http.Request, sess session) {
	ctx := r.Context()
	var req struct {
		Types []string `json:"types"`
		Reset bool     `json:"reset"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req) != nil {
		writeError(w, 400, "Bad request")
		return
	}
	if req.Reset {
		if err := s.store.ResetAcks(ctx, sess.token); err != nil {
			log.Printf("sync: %v", err)
		}
	}
	acks, err := s.store.Acks(ctx, sess.token)
	if err != nil {
		writeError(w, 500, "Internal error")
		return
	}
	want := map[string]bool{}
	for _, t := range req.Types {
		want[t] = true
	}
	w.Header().Set("Content-Type", "application/jsonlines+json")
	out := bufio.NewWriter(w)
	defer out.Flush()
	line := func(kind string, seq int64, data any) error {
		return json.NewEncoder(out).Encode(map[string]any{"type": kind, "data": data, "ack": kind + "|" + strconv.FormatInt(seq, 10)})
	}

	u := sess.user
	person := map[string]any{"id": u.ID, "email": u.Email, "name": u.Name, "deletedAt": nil, "hasProfileImage": false,
		"profileChangedAt": u.CreatedAt, "avatarColor": nil}
	if want["AuthUsersV2"] && u.Seq > acks["AuthUserV2"] {
		auth := map[string]any{"isAdmin": u.IsAdmin, "oauthId": u.Sub, "pinCode": nil, "quotaSizeInBytes": nil, "quotaUsageInBytes": 0, "storageLabel": nil}
		for k, v := range person {
			auth[k] = v
		}
		line("AuthUserV2", u.Seq, auth)
	}
	if want["UsersV1"] && u.Seq > acks["UserV1"] {
		line("UserV1", u.Seq, person)
	}
	gone := func(request, kind, lineType, field string) {
		if err != nil || !want[request] {
			return
		}
		var list []store.Deletion
		if list, err = s.store.DeletionsSince(ctx, u.ID, kind, acks[lineType]); err != nil {
			return
		}
		for _, d := range list {
			if err = line(lineType, d.Seq, map[string]any{field: d.A}); err != nil {
				return
			}
		}
	}
	gone("AssetsV2", "asset", "AssetDeleteV1", "assetId")
	gone("AssetFacesV3", "face", "AssetFaceDeleteV1", "assetFaceId")
	gone("PeopleV1", "person", "PersonDeleteV1", "personId")
	if err == nil && want["AssetsV2"] {
		err = s.store.AssetsSince(ctx, u.ID, acks["AssetV2"], func(a store.Asset) error {
			return line("AssetV2", a.Seq, assetData(a))
		})
	}
	if err == nil && want["AssetExifsV1"] {
		err = s.store.AssetsSince(ctx, u.ID, acks["AssetExifV1"], func(a store.Asset) error {
			return line("AssetExifV1", a.Seq, exifData(a))
		})
	}
	if err == nil {
		err = s.syncAlbums(r, u, want, acks, line)
	}
	if err == nil {
		err = s.syncMemories(r, u, want, acks, line)
	}
	if err == nil && want["PeopleV1"] {
		err = s.store.PeopleSince(ctx, u.ID, acks["PersonV1"], func(p store.Person) error { return line("PersonV1", p.Seq, personData(p)) })
	}
	if err == nil && want["AssetFacesV3"] {
		err = s.store.FacesSince(ctx, u.ID, acks["AssetFaceV3"], func(f store.Face) error { return line("AssetFaceV3", f.Seq, faceData(f)) })
	}
	if err != nil {
		log.Printf("sync: %v", err) // the stream is already under way; the app asks again from its last ack
		return
	}
	line("SyncCompleteV1", 0, map[string]any{})
}

// assetData is an asset as the sync stream and the upload event describe it (SyncAssetV2).
// Until the file has been read, the time is the one the phone sent; afterwards it is the file's own.
func assetData(a store.Asset) map[string]any {
	var duration, width, height, deletedAt any
	if a.DurationMs > 0 {
		duration = a.DurationMs
	}
	if a.DeletedAt != "" {
		deletedAt = a.DeletedAt
	}
	// a live photo: the photo names its moving half, and the moving half is not to be listed by itself
	var live any
	visibility := "timeline"
	if a.Live != "" {
		live = a.Live
	}
	if a.LiveOf != "" {
		visibility = "hidden"
	}
	taken, local := a.FileCreatedAt, wallClock(a.FileCreatedAt)
	if m := a.Meta; m != nil {
		if m.TakenAt != "" {
			taken, local = m.TakenAt, m.LocalTime
		}
		if m.Width > 0 && m.Height > 0 {
			width, height = m.Width, m.Height
			if m.Orientation >= 5 { // stored lying on its side
				width, height = m.Height, m.Width
			}
		}
	}
	return map[string]any{
		"id": a.ID, "ownerId": a.OwnerID, "checksum": base64.StdEncoding.EncodeToString(a.Checksum), "originalFileName": a.OriginalName,
		"type": a.Type, "visibility": visibility, "isFavorite": a.Favorite, "isEdited": false,
		"fileCreatedAt": taken, "fileModifiedAt": a.FileModifiedAt, "localDateTime": local,
		"createdAt": a.CreatedAt, "deletedAt": deletedAt, "duration": duration,
		"width": width, "height": height, "thumbhash": nil, "libraryId": nil, "livePhotoVideoId": live, "stackId": nil}
}

// assetInfo is GET /api/assets/{id}: one photo as AssetResponseDto. The app asks it before casting, for
// the type of the file to tell the television, and casts nothing without it.
func (s *Server) assetInfo(w http.ResponseWriter, r *http.Request, sess session) {
	asset, err := s.store.AssetFor(r.Context(), sess.user.ID, r.PathValue("id")) // one's own, or one that is shared
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, 404, "Not found")
		return
	}
	if err != nil {
		writeError(w, 500, "Internal error")
		return
	}
	body := assetData(asset)
	delete(body, "deletedAt")
	delete(body, "stackId")
	body["originalMimeType"], body["originalPath"], body["updatedAt"] = mimeOf(asset.OriginalName), asset.Path, asset.CreatedAt
	body["hasMetadata"], body["isArchived"], body["isOffline"], body["isTrashed"] = asset.Meta != nil, false, false, asset.DeletedAt != ""
	body["duplicateId"], body["stack"], body["people"], body["tags"] = nil, nil, []any{}, []any{}
	writeJSON(w, 200, body)
}

// mimeOf names the type of a file by its name, including the kinds phones make that Go does not know.
func mimeOf(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".heic":
		return "image/heic"
	case ".heif":
		return "image/heif"
	case ".mov":
		return "video/quicktime"
	case ".dng":
		return "image/x-adobe-dng"
	}
	if t := mime.TypeByExtension(ext); t != "" {
		t, _, _ = strings.Cut(t, ";")
		return t
	}
	return "application/octet-stream"
}

// wallClock turns a UTC instant into the clock time of this machine's zone, written as if it were UTC —
// the form the app groups days by.
func wallClock(utc string) string {
	at, err := time.Parse(time.RFC3339Nano, utc)
	if err != nil {
		return utc
	}
	at = at.Local()
	return store.Timestamp(time.Date(at.Year(), at.Month(), at.Day(), at.Hour(), at.Minute(), at.Second(), at.Nanosecond(), time.UTC))
}

// exifData is SyncAssetExifV1. Fields the file does not state stay null.
func exifData(a store.Asset) map[string]any {
	exif := fixture("exif")
	exif["assetId"], exif["fileSizeInByte"], exif["dateTimeOriginal"] = a.ID, a.Size, a.FileCreatedAt
	m := a.Meta
	if m == nil {
		return exif
	}
	set := func(key string, value any, stated bool) {
		if stated {
			exif[key] = value
		}
	}
	set("dateTimeOriginal", m.TakenAt, m.TakenAt != "")
	set("make", m.Make, m.Make != "")
	set("model", m.Model, m.Model != "")
	set("lensModel", m.Lens, m.Lens != "")
	set("orientation", strconv.Itoa(m.Orientation), m.Orientation != 0)
	set("exifImageWidth", m.Width, m.Width != 0)
	set("exifImageHeight", m.Height, m.Height != 0)
	set("fNumber", m.FNumber, m.FNumber != 0)
	set("focalLength", m.FocalLength, m.FocalLength != 0)
	set("iso", m.ISO, m.ISO != 0)
	set("fps", m.FPS, m.FPS != 0)
	set("city", m.City, m.City != "")
	set("state", m.State, m.State != "")
	set("country", m.Country, m.Country != "")
	if m.ExposureTime >= 1 {
		exif["exposureTime"] = strconv.FormatFloat(m.ExposureTime, 'f', -1, 64)
	} else if m.ExposureTime > 0 {
		exif["exposureTime"] = "1/" + strconv.Itoa(int(math.Round(1/m.ExposureTime)))
	}
	if m.Lat != nil && m.Lon != nil {
		exif["latitude"], exif["longitude"] = *m.Lat, *m.Lon
	}
	if z := m.ZoneSeconds; z != nil {
		zone := "UTC" + fmt.Sprintf("%+d", *z/3600)
		if minutes := *z % 3600 / 60; minutes != 0 {
			zone += fmt.Sprintf(":%02d", max(minutes, -minutes))
		}
		exif["timeZone"] = zone
	}
	return exif
}

// ReadMetadata is the "metadata" job: read what the file says about itself and record it
// (stow issue #6). A file in a format this program does not read is done with nothing recorded;
// a damaged one fails the job.
func (s *Server) ReadMetadata(ctx context.Context, assetID string) error {
	asset, err := s.store.AssetByID(ctx, assetID)
	if err != nil {
		return err
	}
	file := s.lib.Path(asset.Location, asset.Path)
	info, err := media.Read(file, time.Local)
	if err != nil {
		return err
	}
	if info.Kind == "" {
		return nil
	}
	if info.Taken.IsZero() {
		borrowFromOriginal(file, &info)
	}
	meta := store.Meta{Width: info.Width, Height: info.Height, Orientation: info.Orientation, ISO: info.ISO,
		Make: info.Make, Model: info.Model, Lens: info.Lens,
		FNumber: info.FNumber, ExposureTime: info.ExposureTime, FocalLength: info.FocalLength, FPS: info.FPS}
	if !info.Taken.IsZero() {
		meta.TakenAt, meta.LocalTime = store.Timestamp(info.Taken), store.Timestamp(info.Wall)
	}
	if info.HasZone {
		meta.ZoneSeconds = &info.Zone
	}
	if info.HasGPS {
		meta.Lat, meta.Lon = &info.Lat, &info.Lon
	}
	if err := s.store.SetMeta(ctx, assetID, meta, info.DurationMs); err != nil {
		return err
	}
	asset.DurationMs = info.DurationMs
	s.pairLive(ctx, asset)
	if info.HasGPS {
		if err := s.store.AddJob(ctx, assetID, "place"); err != nil {
			return err
		}
		s.runner.Wake()
	}
	if asset.Type == "IMAGE" && asset.LiveOf == "" && asset.DeletedAt == "" {
		if err := s.queueCoupon(ctx, assetID, info.Make); err != nil {
			return err
		}
	}
	s.warnIfLocationIsOff(ctx, asset.OwnerID)
	s.hub.emit(asset.OwnerID, "on_asset_update", map[string]any{"id": assetID}) // the app answers this with a sync
	return nil
}

// appleEdit is how an iPhone names the edited copy of a photo it exports beside the original: IMG_E1234
// for IMG_1234.
var appleEdit = regexp.MustCompile(`^(?i:IMG_E)(\d{4})\.\w+$`)

// borrowFromOriginal fills in, for an iPhone's edited copy, what the edit stripped: when it was taken,
// where, and with what — from the original lying beside it. Without it the copy shows on the day it was
// exported (the file's date), apart from the original, and out of the places (seen: 22 edited copies of
// May 2015 shown on 29 August 2026).
func borrowFromOriginal(file string, info *media.Info) {
	m := appleEdit.FindStringSubmatch(filepath.Base(file))
	if m == nil {
		return
	}
	for _, ext := range []string{".JPG", ".jpg", ".HEIC", ".heic", ".JPEG", ".jpeg", ".PNG", ".png"} {
		orig, err := media.Read(filepath.Join(filepath.Dir(file), "IMG_"+m[1]+ext), time.Local)
		if err != nil || orig.Taken.IsZero() {
			continue
		}
		info.Taken, info.Wall, info.Zone, info.HasZone = orig.Taken, orig.Wall, orig.Zone, orig.HasZone
		if !info.HasGPS && orig.HasGPS {
			info.Lat, info.Lon, info.HasGPS = orig.Lat, orig.Lon, true
		}
		if info.Make == "" { // a camera's photo, not a screenshot: no coupon to look for
			info.Make, info.Model, info.Lens = orig.Make, orig.Model, orig.Lens
		}
		return
	}
}

// NamePlace is the "place" job: turn the coordinates into the name of a town or district, using only
// the data inside the program (stow issue #9). Coordinates far from any settlement get no name.
func (s *Server) NamePlace(ctx context.Context, assetID string) error {
	asset, err := s.store.AssetByID(ctx, assetID)
	if err != nil {
		return err
	}
	if asset.Meta == nil || asset.Meta.Lat == nil || asset.Meta.Lon == nil {
		return nil
	}
	place, ok := places.Lookup(*asset.Meta.Lat, *asset.Meta.Lon)
	if !ok {
		return nil
	}
	if err := s.store.SetPlace(ctx, assetID, place.City, place.State, place.Country); err != nil {
		return err
	}
	s.hub.emit(asset.OwnerID, "on_asset_update", map[string]any{"id": assetID})
	return nil
}

// locationOffAfter is how many camera photos without a single location it takes to say so.
const locationOffAfter = 10

// warnIfLocationIsOff notices, while photos are still coming in, that a person's camera is not recording
// where photos are taken — a setting that cannot be made up for afterwards (stow issue #1). For now the
// notice goes to the log, once per person per run; where the person should see it is not decided.
func (s *Server) warnIfLocationIsOff(ctx context.Context, ownerID string) {
	camera, located, err := s.store.LocationCounts(ctx, ownerID)
	if err != nil || camera < locationOffAfter || located > 0 {
		return
	}
	if _, told := s.warned.LoadOrStore(ownerID, true); !told {
		log.Printf("notice: %d camera photos from user %s and none says where it was taken — the camera's location tag looks switched off", camera, ownerID)
	}
}

var ackKinds = map[string]bool{"AuthUserV2": true, "UserV1": true, "AssetV2": true, "AssetExifV1": true, "PersonV1": true, "AssetFaceV3": true,
	"AssetDeleteV1": true, "AssetFaceDeleteV1": true, "PersonDeleteV1": true,
	"AlbumV2": true, "AlbumDeleteV1": true, "AlbumUserV1": true, "AlbumToAssetV1": true, "AlbumToAssetDeleteV1": true,
	"MemoryV1": true, "MemoryV2": true, "MemoryToAssetV1": true, "MemoryToAssetV2": true, "MemoryDeleteV1": true}

func (s *Server) syncAck(w http.ResponseWriter, r *http.Request, sess session) {
	var req struct {
		Acks []string `json:"acks"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req) != nil {
		writeError(w, 400, "Bad request")
		return
	}
	for _, ack := range req.Acks {
		kind, seqText, _ := strings.Cut(ack, "|")
		seq, err := strconv.ParseInt(seqText, 10, 64)
		if !ackKinds[kind] || err != nil {
			continue
		}
		if err := s.store.SetAck(r.Context(), sess.token, kind, seq); err != nil {
			log.Printf("sync ack: %v", err)
			writeError(w, 500, "Internal error")
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
