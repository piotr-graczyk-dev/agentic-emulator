package main

import (
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/android/emupb"
)

type bearer string

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}
func (bearer) RequireTransportSecurity() bool { return false }

// Frame is one recorded screen frame.
type Frame struct {
	T    float64 `json:"t"` // seconds since the task started (negative: lead-in)
	File string  `json:"file"`
}

type frameRecorder struct {
	mu     sync.Mutex
	t0     time.Time
	start  time.Time
	index  []Frame
	w, h   int
	sw, sh int // device screen
	done   chan struct{}
}

func (f *frameRecorder) setT0(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t0 = t
	for i := range f.index { // frames before t0 were stamped from the recorder's start
		f.index[i].T -= t.Sub(f.start).Seconds()
	}
}

func (f *frameRecorder) wait() { <-f.done }

// startFrames subscribes to the emulator's screenshot stream (frames arrive when the screen
// changes) and writes each frame as a JPEG, stamped with the host time it arrived.
func startFrames(ctx context.Context, emu emulator, width int, dir string) (*frameRecorder, error) {
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(256 << 20)),
	}
	if emu.GRPCToken != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(bearer(emu.GRPCToken)))
	}
	conn, err := grpc.NewClient(fmt.Sprintf("127.0.0.1:%d", emu.GRPCPort), opts...)
	if err != nil {
		return nil, err
	}
	ctl := emupb.NewEmulatorControllerClient(conn)

	// The screen size, from the device (adb), to keep the aspect ratio.
	size, _ := exec.Command(filepath.Join(androidSDK(), "platform-tools", "adb"), "-s", emu.serial(), "shell", "wm", "size").Output()
	sw, sh := 1080, 2424
	if i := strings.LastIndex(string(size), ": "); i >= 0 {
		if a, b, ok := strings.Cut(strings.TrimSpace(string(size)[i+2:]), "x"); ok {
			sw, _ = strconv.Atoi(a)
			sh, _ = strconv.Atoi(b)
		}
	}
	h := width * sh / sw
	h -= h % 2
	st, err := ctl.StreamScreenshot(ctx, &emupb.ImageFormat{Format: emupb.ImageFormat_RGBA8888, Width: uint32(width), Height: uint32(h)})
	if err != nil {
		conn.Close()
		return nil, err
	}
	f := &frameRecorder{start: time.Now(), done: make(chan struct{}), w: width, h: h, sw: sw, sh: sh}
	type job struct {
		img  *emupb.Image
		file string
	}
	jobs := make(chan job, 256)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				w, hh := int(j.img.GetFormat().GetWidth()), int(j.img.GetFormat().GetHeight())
				px := j.img.GetImage()
				if w*hh*4 > len(px) || w == 0 {
					continue
				}
				rgba := &image.RGBA{Pix: px[:w*hh*4], Stride: w * 4, Rect: image.Rect(0, 0, w, hh)}
				out, err := os.Create(filepath.Join(dir, j.file))
				if err != nil {
					continue
				}
				_ = jpeg.Encode(out, rgba, &jpeg.Options{Quality: 88})
				out.Close()
			}
		}()
	}
	go func() {
		defer close(f.done)
		defer conn.Close()
		n := 0
		for {
			img, err := st.Recv()
			if err != nil {
				break
			}
			now := time.Now()
			f.mu.Lock()
			ref := f.start
			if !f.t0.IsZero() {
				ref = f.t0
			}
			n++
			name := fmt.Sprintf("%05d.jpg", n)
			f.index = append(f.index, Frame{T: now.Sub(ref).Seconds(), File: name})
			f.mu.Unlock()
			jobs <- job{img: img, file: name}
		}
		close(jobs)
		wg.Wait()
	}()
	return f, nil
}

// Sample is the CPU and memory of the emulator and of the control plane over one interval.
type Sample struct {
	T          float64 `json:"t"`
	EmuCPU     float64 `json:"emu_cpu"`          // % of one core
	EmuRSSMB   float64 `json:"emu_rss_mb"`       // resident memory (ps; misses most guest RAM)
	EmuFootMB  float64 `json:"emu_footprint_mb"` // phys_footprint (footprint -p), refreshed every 2 s
	PlaneCPU   float64 `json:"plane_cpu"`        // adb server, or agemud
	PlaneRSSMB float64 `json:"plane_rss_mb"`
}

type sampler struct {
	mu   sync.Mutex
	t0   time.Time
	rows []Sample
	done chan struct{}
}

func (s *sampler) setT0(t time.Time) { s.mu.Lock(); s.t0 = t; s.mu.Unlock() }
func (s *sampler) wait()             { <-s.done }

// startSampler reads cumulative CPU time and RSS with ps every 500 ms.
func startSampler(ctx context.Context, pids []int, emuPID int) *sampler {
	s := &sampler{done: make(chan struct{})}
	args := []string{"-o", "pid=,time=,rss="}
	for _, p := range pids {
		args = append(args, "-p", strconv.Itoa(p))
	}
	read := func() map[int][2]float64 {
		out, _ := exec.Command("ps", args...).Output()
		m := map[int][2]float64{}
		for _, line := range strings.Split(string(out), "\n") {
			f := strings.Fields(line)
			if len(f) != 3 {
				continue
			}
			pid, _ := strconv.Atoi(f[0])
			rss, _ := strconv.ParseFloat(f[2], 64)
			m[pid] = [2]float64{cpuSeconds(f[1]), rss / 1024}
		}
		return m
	}
	var foot float64
	var footMu sync.Mutex
	go func() { // footprint takes about 0.1 s, so it runs apart from the CPU samples
		for ctx.Err() == nil {
			if v := footprintMB(emuPID); v > 0 {
				footMu.Lock()
				foot = v
				footMu.Unlock()
			}
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}()
	go func() {
		defer close(s.done)
		prev, prevT := read(), time.Now()
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			cur, now := read(), time.Now()
			dt := now.Sub(prevT).Seconds()
			var row Sample
			for pid, v := range cur {
				cpu := 100 * (v[0] - prev[pid][0]) / dt
				if pid == emuPID {
					row.EmuCPU, row.EmuRSSMB = cpu, v[1]
				} else {
					row.PlaneCPU += cpu
					row.PlaneRSSMB += v[1]
				}
			}
			footMu.Lock()
			row.EmuFootMB = foot
			footMu.Unlock()
			s.mu.Lock()
			if !s.t0.IsZero() {
				row.T = now.Sub(s.t0).Seconds()
				s.rows = append(s.rows, row)
			}
			s.mu.Unlock()
			prev, prevT = cur, now
		}
	}()
	return s
}

var footRe = regexp.MustCompile(`Footprint: ([0-9.]+) (KB|MB|GB)`)

// footprintMB is the process's phys_footprint, as footprint(1) reports it.
func footprintMB(pid int) float64 {
	out, _ := exec.Command("footprint", "-p", strconv.Itoa(pid)).Output()
	m := footRe.FindStringSubmatch(string(out))
	if m == nil {
		return 0
	}
	v, _ := strconv.ParseFloat(m[1], 64)
	switch m[2] {
	case "KB":
		v /= 1024
	case "GB":
		v *= 1024
	}
	return v
}

// cpuSeconds parses ps's cumulative time: [[dd-]hh:]mm:ss.cc.
func cpuSeconds(s string) float64 {
	days := 0.0
	if d, rest, ok := strings.Cut(s, "-"); ok {
		days, _ = strconv.ParseFloat(d, 64)
		s = rest
	}
	total := 0.0
	for _, part := range strings.Split(s, ":") {
		v, _ := strconv.ParseFloat(part, 64)
		total = total*60 + v
	}
	return days*86400 + total
}
