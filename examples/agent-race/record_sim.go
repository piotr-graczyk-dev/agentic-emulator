package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// frameSource is a screen recording: Android's gRPC frame stream or the Simulator's own video.
type frameSource interface {
	setT0(time.Time)
	wait()
	frames() []Frame
	size() (w, h, screenW, screenH int)
}

func (f *frameRecorder) frames() []Frame            { return f.index }
func (f *frameRecorder) size() (int, int, int, int) { return f.w, f.h, f.sw, f.sh }

// iosRecorder records a simulator with `simctl io recordVideo` (CoreSimulator's framebuffer, on
// the host; the same for every arm) and cuts the video into frames at 20 fps afterwards.
type iosRecorder struct {
	cmd      *exec.Cmd
	video    string
	dir      string
	width    int
	recStart time.Time
	t0       time.Time
	index    []Frame
	w, h     int
	sw, sh   int
	done     chan struct{}
}

const iosFPS = 20

func startIOSRecording(ctx context.Context, udid string, width int, dir string) (*iosRecorder, error) {
	r := &iosRecorder{video: filepath.Join(filepath.Dir(dir), "screen.mp4"), dir: dir, width: width, done: make(chan struct{})}
	r.cmd = exec.Command("xcrun", "simctl", "io", udid, "recordVideo", "--codec=h264", "--force", r.video)
	stderr, err := r.cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	r.cmd.Stdout = r.cmd.Stderr
	if err := r.cmd.Start(); err != nil {
		return nil, err
	}
	started := make(chan time.Time, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			if strings.Contains(sc.Text(), "Recording started") {
				started <- time.Now()
			}
		}
	}()
	select {
	case r.recStart = <-started:
	case <-time.After(15 * time.Second):
		_ = r.cmd.Process.Kill()
		return nil, fmt.Errorf("simctl recordVideo did not start")
	}
	go func() {
		defer close(r.done)
		<-ctx.Done()
		_ = r.cmd.Process.Signal(syscall.SIGINT)
		_ = r.cmd.Wait()
		r.extract()
	}()
	return r, nil
}

func (r *iosRecorder) setT0(t time.Time) { r.t0 = t }
func (r *iosRecorder) wait()             { <-r.done }
func (r *iosRecorder) frames() []Frame   { return r.index }
func (r *iosRecorder) size() (int, int, int, int) {
	return r.w, r.h, r.sw, r.sh
}

func (r *iosRecorder) extract() {
	out, _ := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=width,height",
		"-of", "csv=p=0", r.video).Output()
	if a, b, ok := strings.Cut(strings.TrimSpace(string(out)), ","); ok {
		r.sw, _ = strconv.Atoi(a)
		r.sh, _ = strconv.Atoi(b)
	}
	if r.sw == 0 {
		return
	}
	r.w = r.width
	r.h = r.w * r.sh / r.sw
	r.h -= r.h % 2
	_ = os.MkdirAll(r.dir, 0o755)
	if err := exec.Command("ffmpeg", "-loglevel", "error", "-y", "-i", r.video,
		"-vf", fmt.Sprintf("fps=%d,scale=%d:%d", iosFPS, r.w, r.h), "-q:v", "3",
		filepath.Join(r.dir, "%05d.jpg")).Run(); err != nil {
		return
	}
	files, _ := filepath.Glob(filepath.Join(r.dir, "*.jpg"))
	off := r.recStart.Sub(r.t0).Seconds()
	for i := range files {
		r.index = append(r.index, Frame{T: off + float64(i)/iosFPS, File: fmt.Sprintf("%05d.jpg", i+1)})
	}
}
