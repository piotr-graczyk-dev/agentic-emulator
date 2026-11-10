//go:build !windows

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

// Sample is the CPU and memory of the device and of the tool's control plane over one interval.
type Sample struct {
	T          float64 `json:"t"`
	EmuCPU     float64 `json:"emu_cpu"`          // device: % of one core (the emulator, or the simulator's processes)
	EmuRSSMB   float64 `json:"emu_rss_mb"`       // device: resident memory (ps; misses most of an emulator's guest RAM)
	EmuFootMB  float64 `json:"emu_footprint_mb"` // device: phys_footprint (footprint), refreshed every 2 s
	PlaneCPU   float64 `json:"plane_cpu"`        // the tool's host processes (adb server, agemud, daemons)
	PlaneRSSMB float64 `json:"plane_rss_mb"`
}

type proc struct {
	ppid int
	cpu  float64 // cumulative CPU-seconds
	rss  float64 // MB
	args string
}

// readProcs lists every process with ps.
func readProcs() map[int]proc {
	out, _ := exec.Command("ps", "-A", "-o", "pid=,ppid=,time=,rss=,args=").Output()
	m := map[int]proc{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		ppid, _ := strconv.Atoi(f[1])
		rss, _ := strconv.ParseFloat(f[3], 64)
		m[pid] = proc{ppid: ppid, cpu: cpuSeconds(f[2]), rss: rss / 1024, args: strings.Join(f[4:], " ")}
	}
	return m
}

// procSel picks the device's and the control plane's processes from a process list.
type procSel struct {
	device func(map[int]proc) []int
	plane  func(map[int]proc) []int
}

// byPID selects one process.
func byPID(pid int) func(map[int]proc) []int {
	return func(map[int]proc) []int { return []int{pid} }
}

// simTree selects a booted simulator's processes: its launchd_sim and everything under it.
func simTree(udid string) func(map[int]proc) []int {
	return func(ps map[int]proc) []int {
		root := 0
		for pid, p := range ps {
			if strings.Contains(p.args, "launchd_sim") && strings.Contains(p.args, udid) {
				root = pid
			}
		}
		if root == 0 {
			return nil
		}
		kids := map[int][]int{}
		for pid, p := range ps {
			kids[p.ppid] = append(kids[p.ppid], pid)
		}
		var out []int
		stack := []int{root}
		for len(stack) > 0 {
			pid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			out = append(out, pid)
			stack = append(stack, kids[pid]...)
		}
		return out
	}
}

// byArgs selects processes whose command line contains every part of one of the patterns.
func byArgs(patterns ...[]string) func(map[int]proc) []int {
	self := os.Getpid()
	return func(ps map[int]proc) []int {
		var out []int
		for pid, p := range ps {
			if pid == self || strings.HasPrefix(p.args, "ps ") || strings.HasPrefix(p.args, "pgrep") {
				continue
			}
			for _, pat := range patterns {
				all := true
				for _, part := range pat {
					if !strings.Contains(p.args, part) {
						all = false
						break
					}
				}
				if all {
					out = append(out, pid)
					break
				}
			}
		}
		return out
	}
}

type sampler struct {
	mu   sync.Mutex
	t0   time.Time
	rows []Sample
	done chan struct{}
}

func (s *sampler) setT0(t time.Time) { s.mu.Lock(); s.t0 = t; s.mu.Unlock() }
func (s *sampler) wait()             { <-s.done }

// startSampler reads every process's cumulative CPU time and RSS with ps every 500 ms, and the
// device's phys_footprint every 2 s.
func startSampler(ctx context.Context, sel procSel) *sampler {
	s := &sampler{done: make(chan struct{})}
	var foot float64
	var footMu sync.Mutex
	go func() { // footprint takes 0.1 s or more, so it runs apart from the CPU samples
		for ctx.Err() == nil {
			if v := footprintMB(sel.device(readProcs())); v > 0 {
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
		prev, prevT := readProcs(), time.Now()
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			cur, now := readProcs(), time.Now()
			dt := now.Sub(prevT).Seconds()
			delta := func(pid int) float64 {
				c := cur[pid].cpu
				if p, ok := prev[pid]; ok {
					c -= p.cpu
				}
				return 100 * c / dt
			}
			var row Sample
			for _, pid := range sel.device(cur) {
				row.EmuCPU += delta(pid)
				row.EmuRSSMB += cur[pid].rss
			}
			for _, pid := range sel.plane(cur) {
				row.PlaneCPU += delta(pid)
				row.PlaneRSSMB += cur[pid].rss
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

var footRe = regexp.MustCompile(`\[\d+\]: .*Footprint: ([0-9.]+) (KB|MB|GB)`)

// footprintMB is the processes' summed phys_footprint, as footprint(1) reports it per process.
func footprintMB(pids []int) float64 {
	if len(pids) == 0 {
		return 0
	}
	args := []string{}
	for _, p := range pids {
		args = append(args, "-p", strconv.Itoa(p))
	}
	out, _ := exec.Command("footprint", args...).Output()
	total := 0.0
	for _, m := range footRe.FindAllStringSubmatch(string(out), -1) {
		v, _ := strconv.ParseFloat(m[1], 64)
		switch m[2] {
		case "KB":
			v /= 1024
		case "GB":
			v *= 1024
		}
		total += v
	}
	return total
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
