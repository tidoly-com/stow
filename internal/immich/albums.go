package immich

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/tidoly-com/stow/internal/store"
)

// Albums as the Immich app makes and changes them (stow issue #12). An album holds its owner's photos
// only; albums seen by several people are stow issue #13.

// albumBody is AlbumResponseDto.
func albumBody(b store.Album, owner store.User) map[string]any {
	var cover any
	if b.CoverID != "" {
		cover = b.CoverID
	}
	return map[string]any{"id": b.ID, "albumName": b.Name, "description": b.Description, "albumThumbnailAssetId": cover,
		"assetCount": b.Count, "createdAt": b.CreatedAt, "updatedAt": b.UpdatedAt, "order": "desc",
		"shared": false, "hasSharedLink": false, "isActivityEnabled": false,
		"albumUsers": []any{map[string]any{"role": "owner", "user": map[string]any{"id": owner.ID, "email": owner.Email, "name": owner.Name,
			"avatarColor": "primary", "profileImagePath": "", "profileChangedAt": owner.CreatedAt}}}}
}

// albumData is SyncAlbumV2.
func albumData(b store.Album) map[string]any {
	var cover any
	if b.CoverID != "" {
		cover = b.CoverID
	}
	return map[string]any{"id": b.ID, "name": b.Name, "description": b.Description, "thumbnailAssetId": cover,
		"createdAt": b.CreatedAt, "updatedAt": b.UpdatedAt, "order": "desc", "isActivityEnabled": false}
}

func (s *Server) albumFail(w http.ResponseWriter, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, 404, "Not found") // also the answer for another member's album
		return
	}
	log.Printf("albums: %v", err)
	writeError(w, 500, "Internal error")
}

// albumChanged answers with the album as it now is, and tells the person's other devices to sync.
func (s *Server) albumChanged(w http.ResponseWriter, r *http.Request, sess session, id string, status int) {
	b, err := s.store.Album(r.Context(), sess.user.ID, id)
	if err != nil {
		s.albumFail(w, err)
		return
	}
	s.hub.emit(sess.user.ID, "on_album_update", map[string]any{"id": id})
	writeJSON(w, status, albumBody(b, sess.user))
}

func (s *Server) albumList(w http.ResponseWriter, r *http.Request, sess session) {
	list := []any{}
	err := s.store.AlbumsSince(r.Context(), sess.user.ID, 0, func(b store.Album) error {
		list = append(list, albumBody(b, sess.user))
		return nil
	})
	if err != nil {
		s.albumFail(w, err)
		return
	}
	writeJSON(w, 200, list)
}

func (s *Server) albumGet(w http.ResponseWriter, r *http.Request, sess session) {
	b, err := s.store.Album(r.Context(), sess.user.ID, r.PathValue("id"))
	if err != nil {
		s.albumFail(w, err)
		return
	}
	writeJSON(w, 200, albumBody(b, sess.user))
}

func (s *Server) albumCreate(w http.ResponseWriter, r *http.Request, sess session) {
	var req struct {
		AlbumName, Description string
		AssetIDs               []string `json:"assetIds"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req) != nil {
		writeError(w, 400, "Bad request")
		return
	}
	b, err := s.store.CreateAlbum(r.Context(), sess.user.ID, req.AlbumName, req.Description)
	if err == nil && len(req.AssetIDs) > 0 {
		_, err = s.store.ChangeAlbumAssets(r.Context(), sess.user.ID, b.ID, req.AssetIDs, true)
	}
	if err != nil {
		s.albumFail(w, err)
		return
	}
	s.albumChanged(w, r, sess, b.ID, 201)
}

func (s *Server) albumUpdate(w http.ResponseWriter, r *http.Request, sess session) {
	var req struct {
		AlbumName, Description, AlbumThumbnailAssetID *string
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req) != nil {
		writeError(w, 400, "Bad request")
		return
	}
	if err := s.store.UpdateAlbum(r.Context(), sess.user.ID, r.PathValue("id"), req.AlbumName, req.Description, req.AlbumThumbnailAssetID); err != nil {
		s.albumFail(w, err)
		return
	}
	s.albumChanged(w, r, sess, r.PathValue("id"), 200)
}

func (s *Server) albumDelete(w http.ResponseWriter, r *http.Request, sess session) {
	if err := s.store.DeleteAlbum(r.Context(), sess.user.ID, r.PathValue("id")); err != nil {
		s.albumFail(w, err)
		return
	}
	s.hub.emit(sess.user.ID, "on_album_update", map[string]any{"id": r.PathValue("id")})
	w.WriteHeader(http.StatusNoContent)
}

// albumAssets puts photos in (PUT) or takes them out (DELETE), answering per photo.
func (s *Server) albumAssets(add bool) func(http.ResponseWriter, *http.Request, session) {
	return func(w http.ResponseWriter, r *http.Request, sess session) {
		var req struct {
			IDs []string `json:"ids"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req) != nil {
			writeError(w, 400, "Bad request")
			return
		}
		results, err := s.store.ChangeAlbumAssets(r.Context(), sess.user.ID, r.PathValue("id"), req.IDs, add)
		if err != nil {
			s.albumFail(w, err)
			return
		}
		out := make([]any, len(results))
		for i, why := range results {
			entry := map[string]any{"id": req.IDs[i], "success": why == ""}
			if why != "" {
				entry["error"] = why
			}
			out[i] = entry
		}
		s.hub.emit(sess.user.ID, "on_album_update", map[string]any{"id": r.PathValue("id")})
		writeJSON(w, 200, out)
	}
}

// albumsAddAssets puts the same photos into several albums at once.
func (s *Server) albumsAddAssets(w http.ResponseWriter, r *http.Request, sess session) {
	var req struct {
		AlbumIDs []string `json:"albumIds"`
		AssetIDs []string `json:"assetIds"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req) != nil {
		writeError(w, 400, "Bad request")
		return
	}
	for _, id := range req.AlbumIDs {
		if _, err := s.store.ChangeAlbumAssets(r.Context(), sess.user.ID, id, req.AssetIDs, true); err != nil {
			s.albumFail(w, err)
			return
		}
	}
	s.hub.emit(sess.user.ID, "on_album_update", map[string]any{})
	writeJSON(w, 200, map[string]any{"success": true})
}

// syncAlbums writes what changed about albums: gone ones first, so a device never keeps a photo in an
// album that no longer exists.
// syncMemories sends the memories and their photos, in the version the app asks for.
func (s *Server) syncMemories(r *http.Request, u store.User, want map[string]bool, acks map[string]int64, line func(string, int64, any) error) error {
	ctx := r.Context()
	if want["MemoriesV1"] || want["MemoriesV2"] { // past days' memories, taken off
		gone, err := s.store.DeletionsSince(ctx, u.ID, "memory", acks["MemoryDeleteV1"])
		if err != nil {
			return err
		}
		for _, d := range gone {
			if err := line("MemoryDeleteV1", d.Seq, map[string]any{"memoryId": d.A}); err != nil {
				return err
			}
		}
	}
	for _, v := range []string{"V1", "V2"} {
		if want["Memories"+v] {
			if err := s.store.MemoriesSince(ctx, u.ID, acks["Memory"+v], func(m store.Memory) error { return line("Memory"+v, m.Seq, memoryData(m)) }); err != nil {
				return err
			}
		}
	}
	for _, v := range []string{"V1", "V2"} {
		if want["MemoryToAssets"+v] {
			if err := s.store.MemoryAssetsSince(ctx, u.ID, acks["MemoryToAsset"+v], func(memoryID, assetID string, seq int64) error {
				return line("MemoryToAsset"+v, seq, map[string]any{"memoryId": memoryID, "assetId": assetID})
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Server) syncAlbums(r *http.Request, u store.User, want map[string]bool, acks map[string]int64, line func(string, int64, any) error) error {
	ctx := r.Context()
	if want["AlbumsV2"] {
		gone, err := s.store.DeletionsSince(ctx, u.ID, "album", acks["AlbumDeleteV1"])
		if err != nil {
			return err
		}
		for _, d := range gone {
			if err := line("AlbumDeleteV1", d.Seq, map[string]any{"albumId": d.A}); err != nil {
				return err
			}
		}
		if err := s.store.AlbumsSince(ctx, u.ID, acks["AlbumV2"], func(b store.Album) error { return line("AlbumV2", b.Seq, albumData(b)) }); err != nil {
			return err
		}
	}
	if want["AlbumUsersV1"] { // the app learns whose album it is from here
		if err := s.store.AlbumsSince(ctx, u.ID, acks["AlbumUserV1"], func(b store.Album) error {
			return line("AlbumUserV1", b.Seq, map[string]any{"albumId": b.ID, "userId": b.OwnerID, "role": "owner"})
		}); err != nil {
			return err
		}
	}
	if want["AlbumToAssetsV1"] {
		gone, err := s.store.DeletionsSince(ctx, u.ID, "album_asset", acks["AlbumToAssetDeleteV1"])
		if err != nil {
			return err
		}
		for _, d := range gone {
			if err := line("AlbumToAssetDeleteV1", d.Seq, map[string]any{"albumId": d.A, "assetId": d.B}); err != nil {
				return err
			}
		}
		return s.store.AlbumAssetsSince(ctx, u.ID, acks["AlbumToAssetV1"], func(albumID, assetID string, seq int64) error {
			return line("AlbumToAssetV1", seq, map[string]any{"albumId": albumID, "assetId": assetID})
		})
	}
	return nil
}
