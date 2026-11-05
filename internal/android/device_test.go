//go:build devicetest

// Device microbenchmarks against a running agemu-* emulator (not part of `go test ./...`):
//
//	AGEMU_TEST_AVD=agemu-pixel go test -tags 'noios devicetest' -run TestDeviceScreenshotPaths -v ./internal/android/
package android

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func median(ds []time.Duration) time.Duration {
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)/2]
}

func TestDeviceScreenshotPaths(t *testing.T) {
	avd := env("AGEMU_TEST_AVD", "agemu-pixel")
	r, ok := findRunning(avd)
	if !ok {
		t.Skipf("%s not running", avd)
	}
	e, err := dialEmu(r)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	a, _ := loadAVD(avd)
	path := filepath.Join(t.TempDir(), "fb")
	s, err := openShm(path, a.intCfg("hw.lcd.width"), a.intCfg("hw.lcd.height"))
	if err != nil {
		t.Fatal(err)
	}
	e.fb = s
	ctx := context.Background()
	var grpcT, mmapT, pngT []time.Duration
	for i := 0; i < 15; i++ {
		t0 := time.Now()
		f1, err := e.screenshotBytes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		grpcT = append(grpcT, time.Since(t0))
		t0 = time.Now()
		f2, err := e.screenshotMMAP(ctx)
		if err != nil {
			t.Fatal(err)
		}
		mmapT = append(mmapT, time.Since(t0))
		if f1.W != f2.W || f1.H != f2.H {
			t.Fatalf("size mismatch %dx%d vs %dx%d", f1.W, f1.H, f2.W, f2.H)
		}
		t0 = time.Now()
		var buf bytes.Buffer
		_ = encodePNG(&buf, rgbaImage{W: f2.W, H: f2.H, Pix: f2.Pix})
		pngT = append(pngT, time.Since(t0))
		if i == 0 {
			_ = os.WriteFile(filepath.Join(os.TempDir(), "agemu-mmap-check.png"), buf.Bytes(), 0o644)
		}
	}
	t.Logf("getScreenshot bytes: median %v | mmap: median %v | png encode: median %v", median(grpcT), median(mmapT), median(pngT))
}
