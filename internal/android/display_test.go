package android

import (
	"bytes"
	"image"
	"image/png"
	"math"
	"math/rand"
	"testing"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/profiles"
)

func TestScreenMapMatchesLogicalDisplay(t *testing.T) {
	// Observed on the API 37 emulator: 2080x2856 framebuffer, `wm size 1080x2424`
	// -> dumpsys display mCurrentDisplayRect=Rect(404, 0 - 1676, 2856).
	m := newScreenMap(1080, 2424, 2080, 2856)
	if m.R != image.Rect(404, 0, 1676, 2856) {
		t.Fatalf("rect = %v", m.R)
	}
	// Native profile is the identity.
	if n := newScreenMap(1080, 2424, 1080, 2424); !n.identity() {
		t.Fatalf("native not identity: %+v", n)
	}
}

func TestScreenMapRoundTripAllProfiles(t *testing.T) {
	phys := [][2]int{{1080, 2424}, {1280, 2856}, {720, 1600}}
	for _, ph := range phys {
		for _, p := range profiles.All("android") {
			m := newScreenMap(p.Width, p.Height, ph[0], ph[1])
			if m.R.Min.X < 0 || m.R.Min.Y < 0 || m.R.Max.X > ph[0] || m.R.Max.Y > ph[1] {
				t.Fatalf("%s on %v: rect %v outside framebuffer", p.Name, ph, m.R)
			}
			// Every logical point must map into the display rect and back to within one
			// framebuffer pixel: 1 logical px when the framebuffer is at least as large as
			// the profile, ceil(logical/framebuffer) px when the profile is larger.
			tol := max(1, (p.Width+m.R.Dx()-1)/m.R.Dx())
			for _, pt := range [][2]int{{0, 0}, {p.Width - 1, p.Height - 1}, {p.Width / 2, p.Height / 2}, {p.Width / 3, p.Height / 7}, {692, 307}} {
				if pt[0] >= p.Width || pt[1] >= p.Height {
					continue
				}
				px, py := m.toPhysical(pt[0], pt[1])
				if !(image.Point{px, py}.In(m.R)) {
					t.Fatalf("%s: %v -> %d,%d outside %v", p.Name, pt, px, py, m.R)
				}
				lx, ly := m.toLogical(px, py)
				if abs(lx-pt[0]) > tol || abs(ly-pt[1]) > tol {
					t.Fatalf("%s on %v: %v -> (%d,%d) -> (%d,%d)", p.Name, ph, pt, px, py, lx, ly)
				}
			}
		}
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func TestEncodePNGRoundTrip(t *testing.T) {
	for _, sz := range [][2]int{{1, 1}, {7, 3}, {300, 513}, {1080, 2424}} {
		img := rgbaImage{W: sz[0], H: sz[1], Pix: make([]byte, sz[0]*sz[1]*4)}
		r := rand.New(rand.NewSource(1))
		for i := range img.Pix {
			if i%4 == 3 {
				img.Pix[i] = 255
			} else if (i/4)%17 < 9 {
				img.Pix[i] = byte(i / 4 % 251) // structured
			} else {
				img.Pix[i] = byte(r.Intn(256))
			}
		}
		var buf bytes.Buffer
		if err := encodePNG(&buf, img); err != nil {
			t.Fatal(err)
		}
		dec, err := png.Decode(&buf)
		if err != nil {
			t.Fatalf("%v: decode: %v", sz, err)
		}
		if dec.Bounds().Dx() != sz[0] || dec.Bounds().Dy() != sz[1] {
			t.Fatalf("%v: bounds %v", sz, dec.Bounds())
		}
		for y := 0; y < sz[1]; y += max(1, sz[1]/50) {
			for x := 0; x < sz[0]; x += max(1, sz[0]/50) {
				cr, cg, cb, _ := dec.At(x, y).RGBA()
				o := (y*sz[0] + x) * 4
				if byte(cr>>8) != img.Pix[o] || byte(cg>>8) != img.Pix[o+1] || byte(cb>>8) != img.Pix[o+2] {
					t.Fatalf("%v: pixel %d,%d mismatch", sz, x, y)
				}
			}
		}
	}
}

func TestResize(t *testing.T) {
	src := rgbaImage{W: 4, H: 2, Pix: make([]byte, 4*2*4)}
	for i := range src.Pix {
		src.Pix[i] = 200
	}
	for _, s := range [][2]int{{8, 4}, {2, 1}, {3, 5}} {
		out := resize(src, s[0], s[1])
		if out.W != s[0] || out.H != s[1] || len(out.Pix) != s[0]*s[1]*4 {
			t.Fatalf("resize %v: %dx%d", s, out.W, out.H)
		}
		for _, v := range out.Pix {
			if v != 200 {
				t.Fatalf("resize %v: flat color changed to %d", s, v)
			}
		}
	}
}

func BenchmarkEncodePNG(b *testing.B) {
	img := rgbaImage{W: 1080, H: 2424, Pix: make([]byte, 1080*2424*4)}
	for i := range img.Pix {
		img.Pix[i] = byte((i / 4096) % 7 * 30)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var buf bytes.Buffer
		_ = encodePNG(&buf, img)
	}
}

// TestResizeBoxMatchesReference checks the box filter against a direct per-pixel average
// over the same source box.
func TestResizeBoxMatchesReference(t *testing.T) {
	src := rgbaImage{W: 1080, H: 2400, Pix: make([]byte, 1080*2400*4)}
	for i := range src.Pix {
		src.Pix[i] = byte(i*7 + i/4096)
	}
	for _, s := range [][2]int{{720, 1600}, {540, 1200}, {333, 777}} {
		out := resize(src, s[0], s[1])
		sx, sy := float64(src.W)/float64(s[0]), float64(src.H)/float64(s[1])
		for y := 0; y < s[1]; y += 37 {
			for x := 0; x < s[0]; x += 13 {
				sx0 := int(float64(x) * sx)
				sx1 := max(sx0+1, min(src.W, int(math.Ceil(float64(x+1)*sx))))
				sy0 := int(float64(y) * sy)
				sy1 := max(sy0+1, min(src.H, int(math.Ceil(float64(y+1)*sy))))
				for c := 0; c < 4; c++ {
					var sum, n uint32
					for j := sy0; j < sy1; j++ {
						for i := sx0; i < sx1; i++ {
							sum += uint32(src.Pix[(j*src.W+i)*4+c])
							n++
						}
					}
					if want, got := byte((sum+n/2)/n), out.Pix[(y*out.W+x)*4+c]; want != got {
						t.Fatalf("%v: pixel %d,%d channel %d = %d, want %d", s, x, y, c, got, want)
					}
				}
			}
		}
	}
}
