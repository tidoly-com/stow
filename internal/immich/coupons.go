package immich

import (
	"context"
	"time"

	"github.com/tidoly-com/stow/internal/coupons"
)

// CouponAlbum is what the album the coupons gather in is called when the server makes it.
const CouponAlbum = "쿠폰"

// FindCoupon is the "coupon" job: when a picture without camera information has a barcode, put it in its
// owner's coupon album — with its last day when the coupon helper (macOS) reads one in its words. A
// barcode read only once counts when there is such a day. A picture found once is never put back in after
// its owner takes it out.
func (s *Server) FindCoupon(ctx context.Context, assetID string) error {
	if !s.store.CouponsOn(ctx) {
		return nil
	}
	asset, err := s.store.AssetByID(ctx, assetID)
	if err != nil {
		return err
	}
	if asset.Type != "IMAGE" || asset.LiveOf != "" || asset.DeletedAt != "" || asset.Meta != nil && asset.Meta.Make != "" {
		return nil
	}
	original := s.lib.Path(asset.Location, asset.Path)
	code, twice, err := coupons.Find(original, s.cfg.Thumbs.Path(assetID, "preview")) // the preview for what Go cannot open (HEIC)
	if err != nil || code.Text == "" {
		return err
	}
	expires := "" // not known
	if s.cfg.Coupons != nil {
		lines, err := s.cfg.Coupons.Words(ctx, original)
		if err != nil {
			return err
		}
		if until, ok := coupons.Expiry(lines); ok {
			expires = until.Format(time.DateOnly)
		}
	}
	if !twice && expires == "" {
		return nil
	}
	fresh, err := s.store.AddCoupon(ctx, assetID, code.Text, expires)
	if err != nil || !fresh {
		return err
	}
	album, err := s.store.AutoAlbum(ctx, asset.OwnerID, "coupons", CouponAlbum)
	if err != nil {
		return err
	}
	if _, err := s.store.ChangeAlbumAssets(ctx, asset.OwnerID, album.ID, []string{assetID}, true); err != nil {
		return err
	}
	s.Changed(asset.OwnerID) // the app answers with a sync, which brings the album
	return nil
}

// queueCoupon queues the coupon job for a picture just read, when coupons are being looked for.
func (s *Server) queueCoupon(ctx context.Context, asset, cameraMake string) error {
	if cameraMake != "" || !s.store.CouponsOn(ctx) {
		return nil
	}
	if err := s.store.AddJob(ctx, asset, "coupon"); err != nil {
		return err
	}
	s.runner.Wake()
	return nil
}

// QueueMissingCoupons queues the coupon job for every picture that could be a coupon and was never
// looked at — all of them, the first time coupons are switched on.
func (s *Server) QueueMissingCoupons(ctx context.Context) (queued int, err error) {
	if !s.store.CouponsOn(ctx) {
		return 0, nil
	}
	ids, err := s.store.CouponCandidates(ctx)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := s.store.AddJob(ctx, id, "coupon"); err != nil {
			return queued, err
		}
		queued++
	}
	if queued > 0 {
		s.runner.Wake()
	}
	return queued, nil
}

// CouponDays says whether this server reads until when coupons can be used: it was started with the
// coupon helper.
func (s *Server) CouponDays() bool { return s.cfg.Coupons != nil }

// SetCoupons switches looking for coupons on or off. On, the pictures already there are looked at too.
// Off, nothing more is looked at; the album stays as it is.
func (s *Server) SetCoupons(ctx context.Context, on bool) error {
	if err := s.store.SetCouponsOn(ctx, on); err != nil {
		return err
	}
	_, err := s.QueueMissingCoupons(ctx)
	return err
}
