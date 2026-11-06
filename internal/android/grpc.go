package android

import (
	"context"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/android/emupb"
)

// emuClient is a gRPC connection to one running emulator (EmulatorController + SnapshotService).
type emuClient struct {
	conn     *grpc.ClientConn
	ctl      emupb.EmulatorControllerClient
	snap     emupb.SnapshotServiceClient
	port     int
	pid      int
	fb       *shm // MMAP frame transport (nil: frames come in the gRPC reply)
	fbBroken bool
	stream   *frameStream // latest posted frame (nil: off)
}

type bearer string

func (b bearer) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}
func (bearer) RequireTransportSecurity() bool { return false }

func dialEmu(r runningEmu) (*emuClient, error) {
	if r.GRPCPort == 0 {
		return nil, fmt.Errorf("emulator %s has no gRPC port in its discovery file", r.AVD)
	}
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(256 << 20)),
		// Screenshots are tens of MB; start with large flow-control windows.
		grpc.WithInitialWindowSize(64 << 20),
		grpc.WithInitialConnWindowSize(64 << 20),
	}
	if r.GRPCToken != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(bearer(r.GRPCToken)))
	}
	conn, err := grpc.NewClient(fmt.Sprintf("127.0.0.1:%d", r.GRPCPort), opts...)
	if err != nil {
		return nil, err
	}
	return &emuClient{conn: conn, ctl: emupb.NewEmulatorControllerClient(conn), snap: emupb.NewSnapshotServiceClient(conn), port: r.GRPCPort, pid: r.PID}, nil
}

func (e *emuClient) Close() error {
	e.stream.Close()
	err := e.conn.Close()
	e.fb.Close()
	return err
}

// enableShm switches screenshots to the MMAP transport (AGEMU_ANDROID_SHM=0 disables it) and
// starts the frame stream (AGEMU_ANDROID_STREAM=0 disables it), which uses its own mapping.
func (e *emuClient) enableShm(path string, w, h int) {
	if env("AGEMU_ANDROID_SHM", "1") == "0" || w <= 0 || h <= 0 {
		return
	}
	s, err := openShm(path, w, h)
	if err != nil {
		logf("frame shm %s: %v", path, err)
		return
	}
	e.fb = s
	if env("AGEMU_ANDROID_STREAM", "1") == "0" {
		return
	}
	ss, err := openShm(path+".stream", w, h)
	if err != nil {
		logf("frame stream shm %s: %v", path+".stream", err)
		return
	}
	e.stream = startFrameStream(e.ctl, ss, path, w, h)
}

func (e *emuClient) status(ctx context.Context) (*emupb.EmulatorStatus, error) {
	return e.ctl.GetStatus(ctx, &emptypb.Empty{})
}

// frame is one RGBA8888 framebuffer grab, top row first.
type frame struct {
	W, H int
	Pix  []byte // len W*H*4
}

// shm is a file-backed shared memory region the emulator writes frames into (MMAP image
// transport), so a 10 MB frame does not travel through protobuf and HTTP/2.
type shm struct {
	mu   sync.Mutex
	path string
	data []byte
	size int
}

func (e *emuClient) screenshot(ctx context.Context) (frame, error) {
	if e.fb != nil && !e.fbBroken {
		f, err := e.screenshotMMAP(ctx)
		if err == nil {
			return f, nil
		}
		logf("mmap screenshot failed (%v); using gRPC bytes from now on", err)
		e.fbBroken = true
	}
	return e.screenshotBytes(ctx)
}

func (e *emuClient) screenshotMMAP(ctx context.Context) (frame, error) {
	e.fb.mu.Lock()
	defer e.fb.mu.Unlock()
	if e.fb.data == nil {
		return frame{}, fmt.Errorf("frame mapping closed")
	}
	img, err := e.ctl.GetScreenshot(ctx, &emupb.ImageFormat{
		Format:    emupb.ImageFormat_RGBA8888,
		Transport: &emupb.ImageTransport{Channel: emupb.ImageTransport_MMAP, Handle: "file://" + e.fb.path},
	})
	if err != nil {
		return frame{}, err
	}
	f := img.GetFormat()
	w, h := int(f.GetWidth()), int(f.GetHeight())
	if w == 0 || h == 0 || w*h*4 > e.fb.size {
		return frame{}, fmt.Errorf("mmap frame %dx%d does not fit %d bytes", w, h, e.fb.size)
	}
	if len(img.Image) > 0 { // emulator ignored the transport
		return frame{W: w, H: h, Pix: img.Image[:w*h*4]}, nil
	}
	pix := make([]byte, w*h*4)
	copy(pix, e.fb.data[:w*h*4])
	return frame{W: w, H: h, Pix: pix}, nil
}

// openShm creates (or reuses) the frame file for a device, sized for w x h RGBA.
func openShm(path string, w, h int) (*shm, error) {
	size := w * h * 4
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := f.Truncate(int64(size)); err != nil {
		return nil, err
	}
	data, err := syscall.Mmap(int(f.Fd()), 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return nil, err
	}
	return &shm{path: path, data: data, size: size}, nil
}

// Close unmaps the region; it waits for a reader holding s.mu.
func (s *shm) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data != nil {
		_ = syscall.Munmap(s.data)
		s.data = nil
	}
}

func (e *emuClient) screenshotBytes(ctx context.Context) (frame, error) {
	img, err := e.ctl.GetScreenshot(ctx, &emupb.ImageFormat{Format: emupb.ImageFormat_RGBA8888})
	if err != nil {
		return frame{}, err
	}
	f := img.GetFormat()
	w, h := int(f.GetWidth()), int(f.GetHeight())
	if w == 0 || h == 0 {
		return frame{}, fmt.Errorf("emulator returned an empty screenshot (display off?)")
	}
	if len(img.Image) < w*h*4 {
		return frame{}, fmt.Errorf("short screenshot: %d bytes for %dx%d", len(img.Image), w, h)
	}
	return frame{W: w, H: h, Pix: img.Image[:w*h*4]}, nil
}

func (e *emuClient) touch(ctx context.Context, x, y, pressure int) error {
	_, err := e.ctl.SendTouch(ctx, &emupb.TouchEvent{Touches: []*emupb.Touch{{
		X: int32(x), Y: int32(y), Identifier: 0, Pressure: int32(pressure), TouchMajor: 8,
		Expiration: emupb.Touch_NEVER_EXPIRE,
	}}})
	return err
}

func (e *emuClient) setVM(ctx context.Context, st emupb.VmRunState_RunState) error {
	_, err := e.ctl.SetVmState(ctx, &emupb.VmRunState{State: st})
	return err
}

func (e *emuClient) vmState(ctx context.Context) (emupb.VmRunState_RunState, error) {
	r, err := e.ctl.GetVmState(ctx, &emptypb.Empty{})
	if err != nil {
		return emupb.VmRunState_UNKNOWN, err
	}
	return r.GetState(), nil
}

func (e *emuClient) shutdown(ctx context.Context) error {
	_, err := e.ctl.SetVmState(ctx, &emupb.VmRunState{State: emupb.VmRunState_SHUTDOWN})
	return err
}

func (e *emuClient) saveSnapshot(ctx context.Context, name string) error {
	r, err := e.snap.SaveSnapshot(ctx, &emupb.SnapshotPackage{SnapshotId: name})
	if err != nil {
		return err
	}
	if !r.GetSuccess() {
		return fmt.Errorf("save snapshot %s: %s", name, string(r.GetErr()))
	}
	return nil
}

func (e *emuClient) loadSnapshot(ctx context.Context, name string) error {
	r, err := e.snap.LoadSnapshot(ctx, &emupb.SnapshotPackage{SnapshotId: name})
	if err != nil {
		return err
	}
	if !r.GetSuccess() {
		return fmt.Errorf("load snapshot %s: %s", name, string(r.GetErr()))
	}
	return nil
}

func (e *emuClient) deleteSnapshot(ctx context.Context, name string) error {
	r, err := e.snap.DeleteSnapshot(ctx, &emupb.SnapshotPackage{SnapshotId: name})
	if err != nil {
		return err
	}
	if !r.GetSuccess() {
		return fmt.Errorf("delete snapshot %s: %s", name, string(r.GetErr()))
	}
	return nil
}

func (e *emuClient) listSnapshots(ctx context.Context) ([]string, error) {
	l, err := e.snap.ListSnapshots(ctx, &emupb.SnapshotFilter{StatusFilter: emupb.SnapshotFilter_All})
	if err != nil {
		return nil, err
	}
	var names []string
	for _, s := range l.GetSnapshots() {
		names = append(names, s.GetSnapshotId())
	}
	return names, nil
}

// waitGRPC waits until the emulator answers getStatus.
func (e *emuClient) waitGRPC(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		c, cancel := context.WithTimeout(ctx, time.Second)
		_, err := e.status(c)
		cancel()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("emulator gRPC on port %d not ready: %w", e.port, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}
