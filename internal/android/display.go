package android

import (
	"image"
	"math"
	"runtime"
	"sync"
)

// screenMap relates the logical display (what apps see, what the UI tree reports and what
// agemu screenshots show: the profile's pixel grid) to the emulator's physical framebuffer.
//
// A profile is applied as a display size + density override (`wm size` / `wm density`).
// The emulator's framebuffer keeps its native size; Android's DisplayManager scales the
// logical display to fit it, keeping the aspect ratio and centering it
// (LogicalDisplay.configureDisplayLocked). R is that rectangle inside the framebuffer.
// Touch input arrives in framebuffer coordinates and InputReader maps R back to logical.
type screenMap struct {
	LW, LH int // logical display size (profile pixels)
	PW, PH int // physical framebuffer size
	R      image.Rectangle
}

func newScreenMap(lw, lh, pw, ph int) screenMap {
	m := screenMap{LW: lw, LH: lh, PW: pw, PH: ph}
	if lw <= 0 || lh <= 0 || pw <= 0 || ph <= 0 {
		m.R = image.Rect(0, 0, pw, ph)
		return m
	}
	// Same integer math as frameworks/base LogicalDisplay.configureDisplayLocked.
	var dw, dh int
	if pw*lh < ph*lw {
		dw = pw // letterbox
		dh = lh * pw / lw
	} else {
		dw = lw * ph / lh // pillarbox
		dh = ph
	}
	top := (ph - dh) / 2
	left := (pw - dw) / 2
	m.R = image.Rect(left, top, left+dw, top+dh)
	return m
}

// identity is true when the logical display is the framebuffer 1:1 (the AVD's native profile).
func (m screenMap) identity() bool {
	return m.LW == m.PW && m.LH == m.PH && m.R == image.Rect(0, 0, m.PW, m.PH)
}

// toPhysical maps a logical (screenshot) pixel to the framebuffer pixel that InputReader maps
// back onto it: the center of the logical pixel, projected into R.
func (m screenMap) toPhysical(x, y int) (int, int) {
	if m.identity() || m.LW <= 0 || m.LH <= 0 {
		return clamp(x, 0, m.PW-1), clamp(y, 0, m.PH-1)
	}
	px := m.R.Min.X + int(math.Floor((float64(x)+0.5)*float64(m.R.Dx())/float64(m.LW)))
	py := m.R.Min.Y + int(math.Floor((float64(y)+0.5)*float64(m.R.Dy())/float64(m.LH)))
	return clamp(px, m.R.Min.X, m.R.Max.X-1), clamp(py, m.R.Min.Y, m.R.Max.Y-1)
}

// toLogical is the inverse (InputReader's mapping), used in tests.
func (m screenMap) toLogical(px, py int) (int, int) {
	if m.identity() {
		return px, py
	}
	x := int(float64(px-m.R.Min.X) * float64(m.LW) / float64(m.R.Dx()))
	y := int(float64(py-m.R.Min.Y) * float64(m.LH) / float64(m.R.Dy()))
	return x, y
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// rgbaImage is a minimal RGBA8888 buffer (stride = 4*W).
type rgbaImage struct {
	W, H int
	Pix  []byte
}

// crop returns the sub-rectangle r of f (copy-free when r covers whole rows).
func (f frame) crop(r image.Rectangle) rgbaImage {
	r = r.Intersect(image.Rect(0, 0, f.W, f.H))
	if r.Min.X == 0 && r.Max.X == f.W {
		return rgbaImage{W: f.W, H: r.Dy(), Pix: f.Pix[r.Min.Y*f.W*4 : r.Max.Y*f.W*4]}
	}
	out := rgbaImage{W: r.Dx(), H: r.Dy(), Pix: make([]byte, r.Dx()*r.Dy()*4)}
	parallelRows(out.H, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			src := f.Pix[((r.Min.Y+y)*f.W+r.Min.X)*4:]
			copy(out.Pix[y*out.W*4:(y+1)*out.W*4], src[:out.W*4])
		}
	})
	return out
}

// resize resamples src to w x h. Bilinear for upscaling; for downscaling it averages the
// box of source pixels under each output pixel, so text stays legible.
func resize(src rgbaImage, w, h int) rgbaImage {
	if w == src.W && h == src.H {
		return src
	}
	out := rgbaImage{W: w, H: h, Pix: make([]byte, w*h*4)}
	sx := float64(src.W) / float64(w)
	sy := float64(src.H) / float64(h)
	if sx <= 1 && sy <= 1 {
		resizeBilinear(src, out, sx, sy)
	} else {
		resizeBox(src, out, sx, sy)
	}
	return out
}

func resizeBilinear(src, out rgbaImage, sx, sy float64) {
	// Precompute x taps.
	x0 := make([]int, out.W)
	x1 := make([]int, out.W)
	fx := make([]uint32, out.W) // weight of x1 in 1/256
	for x := 0; x < out.W; x++ {
		p := (float64(x)+0.5)*sx - 0.5
		if p < 0 {
			p = 0
		}
		i := int(p)
		if i >= src.W-1 {
			i, p = src.W-1, float64(src.W-1)
		}
		x0[x] = i * 4
		x1[x] = min(i+1, src.W-1) * 4
		fx[x] = uint32((p - float64(i)) * 256)
	}
	parallelRows(out.H, func(ya, yb int) {
		for y := ya; y < yb; y++ {
			p := (float64(y)+0.5)*sy - 0.5
			if p < 0 {
				p = 0
			}
			j := int(p)
			if j >= src.H-1 {
				j, p = src.H-1, float64(src.H-1)
			}
			fy := uint32((p - float64(j)) * 256)
			r0 := src.Pix[j*src.W*4 : (j+1)*src.W*4]
			r1 := src.Pix[min(j+1, src.H-1)*src.W*4:][:src.W*4]
			o := out.Pix[y*out.W*4 : (y+1)*out.W*4]
			for x := 0; x < out.W; x++ {
				a, b := x0[x], x1[x]
				wx := fx[x]
				for c := 0; c < 4; c++ {
					top := uint32(r0[a+c])*(256-wx) + uint32(r0[b+c])*wx
					bot := uint32(r1[a+c])*(256-wx) + uint32(r1[b+c])*wx
					o[x*4+c] = byte((top*(256-fy) + bot*fy + 32768) >> 16)
				}
			}
		}
	})
}

func resizeBox(src, out rgbaImage, sx, sy float64) {
	// Source column range of every output column, computed once.
	x0 := make([]int32, out.W)
	x1 := make([]int32, out.W)
	for x := 0; x < out.W; x++ {
		a := int(float64(x) * sx)
		b := max(a+1, min(src.W, int(math.Ceil(float64(x+1)*sx))))
		x0[x], x1[x] = int32(a), int32(b)
	}
	parallelRows(out.H, func(ya, yb int) {
		acc := make([]uint32, out.W*4)
		for y := ya; y < yb; y++ {
			sy0 := int(float64(y) * sy)
			sy1 := max(sy0+1, min(src.H, int(math.Ceil(float64(y+1)*sy))))
			clear(acc)
			for j := sy0; j < sy1; j++ {
				row := src.Pix[j*src.W*4 : (j+1)*src.W*4]
				for x := 0; x < out.W; x++ {
					a, b := int(x0[x])*4, int(x1[x])*4
					r, g, bl, al := acc[x*4], acc[x*4+1], acc[x*4+2], acc[x*4+3]
					for i := a; i < b; i += 4 {
						p := row[i : i+4 : i+4]
						r += uint32(p[0])
						g += uint32(p[1])
						bl += uint32(p[2])
						al += uint32(p[3])
					}
					acc[x*4], acc[x*4+1], acc[x*4+2], acc[x*4+3] = r, g, bl, al
				}
			}
			o := out.Pix[y*out.W*4 : (y+1)*out.W*4]
			rows := uint32(sy1 - sy0)
			for x := 0; x < out.W; x++ {
				n := rows * uint32(x1[x]-x0[x])
				h := n / 2
				q := acc[x*4 : x*4+4 : x*4+4]
				oo := o[x*4 : x*4+4 : x*4+4]
				oo[0] = byte((q[0] + h) / n)
				oo[1] = byte((q[1] + h) / n)
				oo[2] = byte((q[2] + h) / n)
				oo[3] = byte((q[3] + h) / n)
			}
		}
	})
}

// parallelRows splits [0,h) into contiguous bands processed concurrently.
func parallelRows(h int, fn func(y0, y1 int)) {
	n := runtime.GOMAXPROCS(0)
	if n > 8 {
		n = 8
	}
	if h < 64 || n == 1 {
		fn(0, h)
		return
	}
	var wg sync.WaitGroup
	band := (h + n - 1) / n
	for y := 0; y < h; y += band {
		y0, y1 := y, min(h, y+band)
		wg.Add(1)
		go func() { defer wg.Done(); fn(y0, y1) }()
	}
	wg.Wait()
}
