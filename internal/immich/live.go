package immich

import (
	"context"
	"log"
	"path"
	"strings"

	"github.com/tidoly-com/stow/internal/store"
)

// A live photo is a photo and a few seconds of video taken with it, kept as two files under one name:
// IMG_0772.HEIC and IMG_0772.MOV. They are put together here, so that the list shows one photo and the
// video plays from it.

const liveMaxMs = 5000 // longer than this, a video beside a photo of the same name is a video of its own

// pairLive puts a file together with its other half, if it has one. It is called once the file has been
// read, for either half: whichever is read second finds the first.
func (s *Server) pairLive(ctx context.Context, asset store.Asset) {
	if asset.LiveOf != "" || asset.Live != "" {
		return
	}
	if asset.Type == "VIDEO" && (asset.DurationMs <= 0 || asset.DurationMs > liveMaxMs) {
		return
	}
	stem := strings.TrimSuffix(asset.Path, path.Ext(asset.Path))
	beside, err := s.store.Siblings(ctx, asset.OwnerID, asset.Location, stem)
	if err != nil {
		log.Printf("live photo: %v", err)
		return
	}
	for _, other := range beside {
		if other.ID == asset.ID || other.Type == asset.Type || other.LiveOf != "" || other.Paired ||
			!strings.EqualFold(strings.TrimSuffix(other.Path, path.Ext(other.Path)), stem) {
			continue
		}
		video, photo, length := asset.ID, other.ID, asset.DurationMs
		if asset.Type == "IMAGE" {
			video, photo, length = other.ID, asset.ID, other.DurationMs
		}
		if length <= 0 || length > liveMaxMs {
			continue
		}
		if err := s.store.SetLive(ctx, asset.OwnerID, video, photo); err != nil {
			log.Printf("live photo: %v", err)
		}
		return
	}
}

// PairLivePhotos goes through the short videos that belong to no photo yet and puts each together with
// the photo of its name, if there is one. Done at start, it covers what was read before this was known.
func (s *Server) PairLivePhotos(ctx context.Context) (paired int, err error) {
	ids, err := s.store.ShortVideos(ctx, liveMaxMs)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		asset, err := s.store.AssetByID(ctx, id)
		if err != nil {
			continue
		}
		s.pairLive(ctx, asset)
		if after, err := s.store.AssetByID(ctx, id); err == nil && after.LiveOf != "" {
			paired++
		}
	}
	return paired, nil
}
