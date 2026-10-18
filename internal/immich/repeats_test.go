package immich

import (
	"context"
	"testing"

	"github.com/tidoly-com/stow/internal/media/mediatest"
	"github.com/tidoly-com/stow/internal/store"
)

// A photo read before a repeated face was taken as one — a collage — is looked at again at start: its faces
// are forgotten, the persons its repeats made go, and it comes out with the face once.
func TestRepeatedFacesReadBeforeAreLookedAtAgain(t *testing.T) {
	withFaces = true
	defer func() { withFaces = false }()
	e := start(t, t.TempDir(), newKey(t))
	ctx := context.Background()
	token := e.login("owner@example.com", "owner")
	owner := e.userID("owner@example.com")
	_, up := e.upload(token, "alice alice alice collage.jpg", mediatest.JPEG(nil, 640, 480))
	collage := up["id"].(string)
	e.settle()
	// as an older version recorded it: every repeat kept, as people of their own
	faces, _ := e.store.FaceDescriptions(ctx, owner)
	if len(faces) != 1 {
		t.Fatalf("read now: %d faces, want 1", len(faces))
	}
	for i := 0; i < 2; i++ {
		f := store.Face{AssetID: collage, OwnerID: owner, Score: 0.9, Embedding: faces[0].Embedding, ImageW: 640, ImageH: 480, X1: 10 + 100*i, Y1: 10, X2: 90 + 100*i, Y2: 90}
		if _, err := e.store.AddFace(ctx, &f, ""); err != nil {
			t.Fatal(err)
		}
	}
	if faces, _ := e.store.FaceDescriptions(ctx, owner); len(faces) != 3 {
		t.Fatalf("as recorded before: %d faces", len(faces))
	}
	n, err := e.api.LookAgainAtRepeats(ctx)
	if err != nil || n != 1 {
		t.Fatalf("looked again at %d photos: %v", n, err)
	}
	e.settle()
	if faces, _ := e.store.FaceDescriptions(ctx, owner); len(faces) != 1 {
		t.Errorf("after: %d faces, want 1", len(faces))
	}
	if n, _ := e.api.LookAgainAtRepeats(ctx); n != 0 {
		t.Errorf("a second look found %d more", n)
	}
}
