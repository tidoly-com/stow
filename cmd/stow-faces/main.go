// stow-faces is the face helper of tidoly stow (stow issue #10): it finds the faces in a picture and describes
// each as 128 numbers, close for the same person. The photo server starts it and speaks to it over standard
// input and output, one JSON object per line each way (internal/faces):
//
//	stow-faces -models <folder holding face_recognition_sface_2021dec.onnx> [-lib <ONNX Runtime library>]
//
// The models are OpenCV's: YuNet finds (MIT; carried inside this program, its input size freed — the
// published file says 640x640, which ONNX Runtime holds to), SFace describes (Apache-2.0; 37 MB, fetched by
// the server on first start). They run on ONNX Runtime (MIT), loaded from the library beside this program.
// What OpenCV does around them is written out here: YuNet's input padding and output decoding with
// non-maximum suppression (opencv/modules/objdetect/src/face_detect.cpp), and SFace's alignment — a
// similarity transform from the five landmarks onto ArcFace's template, then a 112x112 crop
// (face_recognize.cpp). Measured against OpenCV in trials/2026-10-15-faces-onnx: the same faces, and the
// same answers on the Mac and Windows.
package main

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"

	ort "github.com/yalue/onnxruntime_go"
	"golang.org/x/image/draw"
)

const (
	scoreThreshold = 0.8 // as the stand-in
	nmsThreshold   = 0.3 // OpenCV's default
	minSide        = 48  // pixels at the size looked at; smaller faces describe too poorly to tell apart
)

type face struct {
	Box       [4]float64 `json:"box"`
	Score     float64    `json:"score"`
	Embedding []float32  `json:"embedding"`
	side      float64
}

type found struct {
	x, y, w, h, score float32
	marks             [10]float32
}

var detector, recognizer *ort.DynamicAdvancedSession

// yunet is face_detection_yunet_2023mar.onnx from opencv_zoo with its input size made free
// (trials/2026-10-15-faces-onnx/dynamic.py).
//
//go:embed yunet.onnx
var yunet []byte

// library is ONNX Runtime's file name on each system, looked for beside this program.
func library() string {
	self, _ := os.Executable()
	name := map[string]string{"darwin": "libonnxruntime.dylib", "windows": "onnxruntime.dll"}[runtime.GOOS]
	if name == "" {
		name = "libonnxruntime.so"
	}
	return filepath.Join(filepath.Dir(self), name)
}

func main() {
	lib := flag.String("lib", library(), "path to the ONNX Runtime library")
	models := flag.String("models", "", "folder holding face_recognition_sface_2021dec.onnx")
	flag.Parse()
	ort.SetSharedLibraryPath(*lib)
	if err := ort.InitializeEnvironment(); err != nil {
		fail(err)
	}
	opts, err := ort.NewSessionOptions()
	if err != nil {
		fail(err)
	}
	opts.SetIntraOpNumThreads(2) // a helper beside a photo server, not the machine's main work
	// ONNX Runtime's Arm-only kernels (KleidiAI) took 800 MB on the Mac for what Windows did in 220 MB;
	// without them the Mac needs 338 MB, with the same answers (measured 2026-10-15). Ignored elsewhere.
	if err := opts.AddSessionConfigEntry("mlas.disable_kleidiai", "1"); err != nil {
		fail(err)
	}
	opts.SetInterOpNumThreads(1)
	opts.SetCpuMemArena(false) // every picture has its own size: nothing to gain from keeping memory per size
	opts.SetMemPattern(false)
	outs := []string{}
	for _, kind := range []string{"cls", "obj", "bbox", "kps"} {
		for _, s := range []int{8, 16, 32} {
			outs = append(outs, fmt.Sprintf("%s_%d", kind, s))
		}
	}
	if detector, err = ort.NewDynamicAdvancedSessionWithONNXData(yunet, []string{"input"}, outs, opts); err != nil {
		fail(err)
	}
	if recognizer, err = ort.NewDynamicAdvancedSession(filepath.Join(*models, "face_recognition_sface_2021dec.onnx"), []string{"data"}, []string{"fc1"}, opts); err != nil {
		fail(err)
	}

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	out := json.NewEncoder(os.Stdout)
	for in.Scan() {
		var req struct{ Preview, Original string }
		var answer any
		if err := json.Unmarshal(in.Bytes(), &req); err != nil {
			answer = map[string]string{"error": err.Error()}
		} else if faces, err := look(req.Preview, req.Original); err != nil {
			answer = map[string]string{"error": err.Error()}
		} else {
			answer = map[string]any{"faces": faces}
		}
		out.Encode(answer)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

// look finds the faces in the preview, and — as the stand-in — in the original too when it is much larger,
// at a long side of 2880: one size of picture does not find both faces that fill the frame and faces far
// back. Of two sightings of one face the one with more pixels is kept.
func look(preview, original string) ([]face, error) {
	p, err := load(preview)
	if err != nil {
		return nil, fmt.Errorf("cannot read the preview: %w", err)
	}
	faces, err := facesIn(p)
	if err != nil {
		return nil, err
	}
	if o, err := load(original); err == nil && longSide(o) > 1.5*longSide(p) {
		more, err := facesIn(shrink(o, 2880))
		if err != nil {
			return nil, err
		}
		faces = append(faces, more...)
	}
	sort.SliceStable(faces, func(i, j int) bool { return faces[i].side > faces[j].side })
	kept := []face{}
	for _, f := range faces {
		alone := true
		for _, k := range kept {
			if iou(f.Box, k.Box) >= 0.4 {
				alone = false
				break
			}
		}
		if alone {
			kept = append(kept, f)
		}
	}
	return kept, nil
}

// load reads a picture into plain RGBA bytes, which the loops below read directly — through
// image.Image.At they took most of the time.
func load(path string) (*image.RGBA, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, err
	}
	rgba := image.NewRGBA(image.Rect(0, 0, img.Bounds().Dx(), img.Bounds().Dy()))
	draw.Draw(rgba, rgba.Bounds(), img, img.Bounds().Min, draw.Src)
	return rgba, nil
}

func longSide(img *image.RGBA) float64 {
	b := img.Bounds()
	return float64(max(b.Dx(), b.Dy()))
}

func shrink(img *image.RGBA, long int) *image.RGBA {
	b := img.Bounds()
	if max(b.Dx(), b.Dy()) <= long {
		return img
	}
	scale := float64(long) / float64(max(b.Dx(), b.Dy()))
	dst := image.NewRGBA(image.Rect(0, 0, int(math.Round(float64(b.Dx())*scale)), int(math.Round(float64(b.Dy())*scale))))
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), img, b, draw.Src, nil)
	return dst
}

// facesIn runs YuNet on the picture as it is, padded to a multiple of 32, then SFace on each face.
func facesIn(img *image.RGBA) ([]face, error) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	pw, ph := (w+31)/32*32, (h+31)/32*32
	data := make([]float32, 3*pw*ph) // BGR planes, 0-255, zero padding right and below
	for y := 0; y < h; y++ {
		row := img.Pix[y*img.Stride:]
		for x := 0; x < w; x++ {
			p := row[4*x:]
			i := y*pw + x
			data[i], data[pw*ph+i], data[2*pw*ph+i] = float32(p[2]), float32(p[1]), float32(p[0])
		}
	}
	input, err := ort.NewTensor(ort.NewShape(1, 3, int64(ph), int64(pw)), data)
	if err != nil {
		return nil, err
	}
	defer input.Destroy()
	outputs := make([]ort.Value, 12)
	if err := detector.Run([]ort.Value{input}, outputs); err != nil {
		return nil, fmt.Errorf("detector at %dx%d: %w", pw, ph, err)
	}
	defer func() {
		for _, o := range outputs {
			if o != nil {
				o.Destroy()
			}
		}
	}()
	values := func(i int) []float32 { return outputs[i].(*ort.Tensor[float32]).GetData() }
	var all []found
	for si, stride := range []int{8, 16, 32} {
		cls, obj, box, kps := values(si), values(3+si), values(6+si), values(9+si)
		cols, rows := pw/stride, ph/stride
		for r := 0; r < rows; r++ {
			for c := 0; c < cols; c++ {
				i := r*cols + c
				score := float32(math.Sqrt(float64(clamp(cls[i]) * clamp(obj[i]))))
				if score < scoreThreshold {
					continue
				}
				s := float32(stride)
				cx, cy := (float32(c)+box[i*4])*s, (float32(r)+box[i*4+1])*s
				bw, bh := float32(math.Exp(float64(box[i*4+2])))*s, float32(math.Exp(float64(box[i*4+3])))*s
				f := found{x: cx - bw/2, y: cy - bh/2, w: bw, h: bh, score: score}
				for n := 0; n < 5; n++ {
					f.marks[2*n] = (kps[i*10+2*n] + float32(c)) * s
					f.marks[2*n+1] = (kps[i*10+2*n+1] + float32(r)) * s
				}
				all = append(all, f)
			}
		}
	}
	var faces []face
	for _, f := range suppress(all) {
		side := float64(min(f.w, f.h))
		if side < minSide {
			continue
		}
		emb, err := describe(img, f.marks)
		if err != nil {
			return nil, err
		}
		x1, y1 := float64(f.x)/float64(w), float64(f.y)/float64(h)
		x2, y2 := float64(f.x+f.w)/float64(w), float64(f.y+f.h)/float64(h)
		faces = append(faces, face{Box: [4]float64{max(0, x1), max(0, y1), min(1, x2), min(1, y2)}, Score: float64(f.score), Embedding: emb, side: side})
	}
	return faces, nil
}

func clamp(v float32) float32 { return min(max(v, 0), 1) }

// suppress keeps the best of boxes that overlap by more than nmsThreshold.
func suppress(all []found) []found {
	sort.Slice(all, func(i, j int) bool { return all[i].score > all[j].score })
	var kept []found
	for _, f := range all {
		ok := true
		for _, k := range kept {
			if iou([4]float64{float64(f.x), float64(f.y), float64(f.x + f.w), float64(f.y + f.h)},
				[4]float64{float64(k.x), float64(k.y), float64(k.x + k.w), float64(k.y + k.h)}) > nmsThreshold {
				ok = false
				break
			}
		}
		if ok {
			kept = append(kept, f)
		}
	}
	return kept
}

func iou(a, b [4]float64) float64 {
	w, h := min(a[2], b[2])-max(a[0], b[0]), min(a[3], b[3])-max(a[1], b[1])
	if w <= 0 || h <= 0 {
		return 0
	}
	inter := w * h
	return inter / ((a[2]-a[0])*(a[3]-a[1]) + (b[2]-b[0])*(b[3]-b[1]) - inter)
}

// ArcFace's five points in a 112x112 face: the eyes, the nose, the corners of the mouth.
var template = [10]float64{38.2946, 51.6963, 73.5318, 51.5014, 56.0252, 71.7366, 41.5493, 92.3655, 70.7299, 92.2041}

// describe lines the face up on the template and asks SFace for its 128 numbers.
func describe(img *image.RGBA, marks [10]float32) ([]float32, error) {
	a, b, tx, ty := similarity(marks)
	// the transform takes the picture to the template; to fill the crop, each crop pixel looks back
	det := a*a + b*b
	ia, ib := a/det, -b/det            // inverse of [[a -b] [b a]]
	data := make([]float32, 3*112*112) // RGB planes, 0-255 (OpenCV's blobFromImage with swapRB)
	for v := 0; v < 112; v++ {
		for u := 0; u < 112; u++ {
			du, dv := float64(u)-tx, float64(v)-ty
			x, y := ia*du-ib*dv, ib*du+ia*dv
			r, g, bl := bilinear(img, x, y)
			i := v*112 + u
			data[i], data[112*112+i], data[2*112*112+i] = r, g, bl
		}
	}
	input, err := ort.NewTensor(ort.NewShape(1, 3, 112, 112), data)
	if err != nil {
		return nil, err
	}
	defer input.Destroy()
	outputs := []ort.Value{nil}
	if err := recognizer.Run([]ort.Value{input}, outputs); err != nil {
		return nil, err
	}
	defer outputs[0].Destroy()
	return append([]float32(nil), outputs[0].(*ort.Tensor[float32]).GetData()...), nil
}

// similarity is the least-squares rotation, scale and shift taking the five marks onto the template
// (Umeyama, as OpenCV's alignCrop): template ≈ [[a -b] [b a]]·mark + [tx ty].
func similarity(marks [10]float32) (a, b, tx, ty float64) {
	var mx, my, tmx, tmy float64
	for n := 0; n < 5; n++ {
		mx += float64(marks[2*n]) / 5
		my += float64(marks[2*n+1]) / 5
		tmx += template[2*n] / 5
		tmy += template[2*n+1] / 5
	}
	var sxx, sxy, norm float64
	for n := 0; n < 5; n++ {
		x, y := float64(marks[2*n])-mx, float64(marks[2*n+1])-my
		u, v := template[2*n]-tmx, template[2*n+1]-tmy
		sxx += x*u + y*v
		sxy += x*v - y*u
		norm += x*x + y*y
	}
	a, b = sxx/norm, sxy/norm
	tx, ty = tmx-(a*mx-b*my), tmy-(b*mx+a*my)
	return
}

func bilinear(img *image.RGBA, x, y float64) (r, g, b float32) {
	x0, y0 := int(math.Floor(x)), int(math.Floor(y))
	fx, fy := float32(x-float64(x0)), float32(y-float64(y0))
	px := func(x, y int) (float32, float32, float32) {
		if x < 0 || y < 0 || x >= img.Rect.Dx() || y >= img.Rect.Dy() {
			return 0, 0, 0
		}
		p := img.Pix[y*img.Stride+4*x:]
		return float32(p[0]), float32(p[1]), float32(p[2])
	}
	r00, g00, b00 := px(x0, y0)
	r10, g10, b10 := px(x0+1, y0)
	r01, g01, b01 := px(x0, y0+1)
	r11, g11, b11 := px(x0+1, y0+1)
	mix := func(a, b, c, d float32) float32 {
		return (a*(1-fx)+b*fx)*(1-fy) + (c*(1-fx)+d*fx)*fy
	}
	return mix(r00, r10, r01, r11), mix(g00, g10, g01, g11), mix(b00, b10, b01, b11)
}
