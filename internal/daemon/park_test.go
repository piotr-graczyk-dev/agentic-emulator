package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/backend"
)

// parkBackend is a Parker that records the order of its calls.
type parkBackend struct {
	fakeBackend
	parked    bool
	calls     []string
	resumeErr error
}

var _ backend.Parker = (*parkBackend)(nil)

func (p *parkBackend) List(ctx context.Context) ([]api.Device, error) {
	st := "booted"
	if p.parked {
		st = api.StateParked
	}
	return []api.Device{{ID: "android:x", Name: "x", Platform: api.Android, State: st}}, nil
}

func (p *parkBackend) Park(ctx context.Context, id string) error {
	p.calls = append(p.calls, "park")
	p.parked = true
	return nil
}

func (p *parkBackend) Parked(id string) bool { return p.parked }

func (p *parkBackend) Resume(ctx context.Context, id string) (bool, error) {
	p.calls = append(p.calls, "resume")
	if p.resumeErr != nil {
		return true, p.resumeErr
	}
	was := p.parked
	p.parked = false
	return was, nil
}

func (p *parkBackend) UITree(ctx context.Context, id string) (*api.UITree, error) {
	p.calls = append(p.calls, "ui")
	return tree(), nil
}

func (p *parkBackend) Logs(ctx context.Context, id string, q api.LogQuery) (api.LogBatch, error) {
	p.calls = append(p.calls, "logs")
	return api.LogBatch{}, nil
}

func TestACallOnAParkedDeviceResumesItFirst(t *testing.T) {
	pb := &parkBackend{}
	s := NewService(t.TempDir(), pb)
	if _, err := call(t, s, "park", `{"device":"android:x"}`); err != nil || !pb.parked {
		t.Fatalf("park: %v, parked %v", err, pb.parked)
	}
	// No device given: the only device is parked, which counts as booted.
	if _, err := call(t, s, "ui", `{}`); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(pb.calls, ","); got != "park,resume,ui" {
		t.Fatalf("calls = %s, want the resume before the tree read", got)
	}
	// Not parked any more: the next call goes straight to the backend.
	pb.calls = nil
	if _, err := call(t, s, "ui", `{"device":"android:x"}`); err != nil || strings.Join(pb.calls, ",") != "ui" {
		t.Fatalf("calls = %v, err %v", pb.calls, err)
	}
}

func TestReadsOfCapturedLogsDoNotResume(t *testing.T) {
	pb := &parkBackend{parked: true}
	s := NewService(t.TempDir(), pb)
	if _, err := call(t, s, "logs", `{"device":"android:x"}`); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(pb.calls, ","); got != "logs" || !pb.parked {
		t.Fatalf("calls = %s, parked %v; logs must read without resuming", got, pb.parked)
	}
}

func TestResumeReportsWhetherTheDeviceWasParked(t *testing.T) {
	pb := &parkBackend{parked: true}
	s := NewService(t.TempDir(), pb)
	for _, want := range []bool{true, false} {
		res, err := call(t, s, "resume", `{"device":"android:x"}`)
		if err != nil {
			t.Fatal(err)
		}
		if got := res.(map[string]any)["resumed"]; got != want {
			t.Fatalf("resumed = %v, want %v", got, want)
		}
	}
	if got := strings.Join(pb.calls, ","); got != "resume,resume" {
		t.Fatalf("calls = %s: resume must be called once per request, not also by the wake", got)
	}
}

func TestAFailedResumeFailsTheCall(t *testing.T) {
	pb := &parkBackend{parked: true, resumeErr: errors.New("no answer")}
	s := NewService(t.TempDir(), pb)
	if _, err := call(t, s, "ui", `{"device":"android:x"}`); err == nil || !strings.Contains(err.Error(), "no answer") {
		t.Fatalf("err = %v", err)
	}
	if got := strings.Join(pb.calls, ","); got != "resume" {
		t.Fatalf("calls = %s: the tree must not be read on a device that did not resume", got)
	}
}

func TestParkIsNotImplementedWithoutAParker(t *testing.T) {
	s := NewService(t.TempDir(), &fakeBackend{})
	for _, m := range []string{"park", "resume"} {
		if _, err := call(t, s, m, `{"device":"android:x"}`); !errors.Is(err, api.ErrNotImplemented) {
			t.Fatalf("%s: err = %v, want not implemented", m, err)
		}
	}
}
