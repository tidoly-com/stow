// Package faces talks to the face helper: a separate program that finds the faces in a picture and
// describes each as a list of numbers, close for the same person and far apart for different people
// (stow issue #10). The helper is kept apart so that the photo server runs the same with or without it,
// and so that nothing the helper needs — a model runtime, model files — has to live inside the server.
//
// The helper is started on first use and spoken to over its standard input and output, one JSON object
// per line each way:
//
//	-> {"preview": "/path/to/upright-preview.jpg", "original": "/path/to/original"}
//	<- {"faces": [{"box": [x1, y1, x2, y2], "score": 0.91, "embedding": [ ... ]}]}     or {"error": "..."}
//
// Boxes are fractions of the upright picture's width and height.
package faces

import (
	"context"
	"math"
	"time"

	"github.com/tidoly-com/stow/internal/helper"
)

type Face struct {
	Box       [4]float64 `json:"box"`
	Score     float64    `json:"score"`
	Embedding []float32  `json:"embedding"`
}

type Helper struct{ helper.Process }

// New is a face helper started by the given command.
// It ends itself after a few idle minutes — it holds a few hundred MB while it runs — and starts again
// with the next photo.
func New(command []string) *Helper {
	return &Helper{helper.Process{Name: "face helper", Command: command, Idle: 3 * time.Minute}}
}

// Detect asks the helper for the faces in one picture.
func (h *Helper) Detect(ctx context.Context, preview, original string) ([]Face, error) {
	var reply struct {
		Faces []Face `json:"faces"`
	}
	if err := h.Ask(ctx, map[string]string{"preview": preview, "original": original}, &reply); err != nil {
		return nil, err
	}
	for i := range reply.Faces {
		normalize(reply.Faces[i].Embedding)
	}
	return reply.Faces, nil
}

// normalize scales a description to length 1, so that likeness is a plain dot product.
func normalize(v []float32) {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return
	}
	scale := float32(1 / math.Sqrt(sum))
	for i := range v {
		v[i] *= scale
	}
}

// Likeness is how alike two normalized descriptions are: 1 for the same, around 0 for unrelated.
func Likeness(a, b []float32) float64 {
	if len(a) != len(b) {
		return -1
	}
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot
}
