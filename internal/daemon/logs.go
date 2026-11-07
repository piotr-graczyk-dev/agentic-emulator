package daemon

// Logs and crashes (docs/CAPABILITIES.md, "Logs and crashes"):
//   - logs: an app's or the device's recent log lines, filtered and bounded here, so both
//     platforms behave the same (backend.LogReader supplies the lines);
//   - crashes: crash, ANR and system-kill reports (backend.CrashReader);
//   - the crash notice: after every call on a device, the daemon asks the backend's
//     backend.AppWatcher about the app launched last through agemu (launch, or open_url when
//     the backend names the app), and tells the agent once when it crashed; a failed call
//     while the app is down says why (code app_crashed), and a wait for an element stops at
//     once while the app is down after a crash the agent has not been told about.
//
// logs and crashes run without the device lock: they read what the backend captured.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/backend"
)

// devState is what the daemon remembers about one device for logs, crashes and the notice.
type devState struct {
	floor    time.Time            // end of the last reset through agemu: logs and crashes start here
	launched map[string]time.Time // app -> start of its last launch through agemu (since "launch")
	watch    string               // the app launched last through agemu; its crashes are reported
	since    time.Time            // the start of that launch
	reported time.Time            // the time of the newest crash of watch already reported
}

// watchTimeout bounds one AppState call, so a slow watcher cannot hold up every call.
const watchTimeout = 250 * time.Millisecond

// stateLocked returns id's state; the caller holds s.mu.
func (s *Service) stateLocked(id string) *devState {
	st, ok := s.states[id]
	if !ok {
		st = &devState{launched: map[string]time.Time{}}
		s.states[id] = st
	}
	return st
}

// appOf is the app of a launch target: Android's "pkg/activity" names the package.
func appOf(target string) string {
	app, _, _ := strings.Cut(strings.TrimSpace(target), "/")
	return app
}

// launching records that app's launch through agemu starts now: its crashes from now on are
// reported, also one during the launch itself.
func (s *Service) launching(id, app string, at time.Time) {
	if app == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stateLocked(id)
	st.launched[app] = at
	st.watch, st.since, st.reported = app, at, time.Time{}
}

// track updates the device state after a successful action (res is the action's own result).
func (s *Service) track(id, method string, p Params, res any) {
	switch method {
	case "open_url":
		if m, ok := res.(map[string]any); ok {
			if app, _ := m["app"].(string); app != "" {
				s.launching(id, app, p.start)
			}
		}
		return
	case "terminate", "uninstall", "clear_app_data", "reset", "shutdown":
	default:
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stateLocked(id)
	switch method {
	case "terminate", "uninstall", "clear_app_data":
		// An exit agemu asked for is not a crash; stop watching the app.
		if appOf(p.App) == st.watch {
			st.watch = ""
		}
	case "reset":
		*st = devState{floor: time.Now(), launched: map[string]time.Time{}}
	case "shutdown":
		*st = devState{floor: st.floor, launched: map[string]time.Time{}}
	}
}

func (s *Service) floorOf(id string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stateLocked(id).floor
}

// launchTime resolves since "launch": the start of app's last launch through agemu, or of the
// device's last launch when app is "".
func (s *Service) launchTime(id, app string) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stateLocked(id)
	if app == "" {
		if st.watch == "" {
			return time.Time{}, errors.New(`since "launch": no app was launched through agemu on this device since the daemon started or the device was reset; pass app, a time or a duration`)
		}
		return st.since, nil
	}
	t, ok := st.launched[app]
	if !ok {
		return time.Time{}, fmt.Errorf(`since "launch": %s was not launched through agemu on this device since the daemon started or the device was reset; pass a time or a duration`, app)
	}
	return t, nil
}

// appName is what an Android package name (letters, digits, '_', '.') or an iOS bundle id
// (letters, digits, '-', '.') can hold; backends may pass it to a device shell.
var appName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// checkObserve validates and normalizes the arguments of logs and crashes, before anything
// touches the backend. since becomes an absolute time (RFC 3339) or a cursor.
func (s *Service) checkObserve(id, method string, p *Params) error {
	if method != "logs" && method != "crashes" {
		return nil
	}
	p.App = strings.TrimSpace(p.App)
	if p.App != "" && !appName.MatchString(p.App) {
		return fmt.Errorf("%s: app %q is not a package name or bundle id", method, p.App)
	}
	since, err := api.ParseSince(p.Since, time.Now())
	if err != nil {
		return err
	}
	if since.Launch {
		t, err := s.launchTime(id, p.App)
		if err != nil {
			return err
		}
		since.Time = t
	}
	switch {
	case since.Cursor > 0:
		p.Since = api.LogCursor(since.Cursor)
	case !since.Time.IsZero():
		p.Since = since.Time.Format(time.RFC3339Nano)
	default:
		p.Since = ""
	}

	if method == "crashes" {
		if since.Cursor > 0 {
			return errors.New("crashes: since takes a time, a duration or \"launch\", not a logs cursor")
		}
		if p.Level != "" || p.Contains != "" || p.Regex != "" || p.MaxBytes != 0 {
			return errors.New("crashes takes app, since and limit (level, contains, regex and maxBytes are for logs)")
		}
		return checkLimit(method, &p.Limit, api.DefaultCrashLimit, api.MaxCrashLimit)
	}

	if p.Level != "" {
		l, err := api.ParseLogLevel(p.Level)
		if err != nil {
			return err
		}
		p.Level = string(l)
	}
	p.Contains = strings.ToLower(p.Contains)
	if p.Regex != "" {
		if _, err := api.CompileLogRegex(p.Regex); err != nil {
			return err
		}
	}
	if err := checkLimit(method, &p.Limit, api.DefaultLogLimit, api.MaxLogLimit); err != nil {
		return err
	}
	switch {
	case p.MaxBytes == 0:
		p.MaxBytes = api.DefaultLogBytes
	case p.MaxBytes < api.MinLogBytes || p.MaxBytes > api.MaxLogBytes:
		return fmt.Errorf("logs: maxBytes must be between %d and %d, got %d", api.MinLogBytes, api.MaxLogBytes, p.MaxBytes)
	}
	return nil
}

func checkLimit(method string, limit *int, def, max int) error {
	switch {
	case *limit == 0:
		*limit = def
	case *limit < 0 || *limit > max:
		return fmt.Errorf("%s: limit must be between 1 and %d, got %d", method, max, *limit)
	}
	return nil
}

// sinceOf reads a since checked by checkObserve, raised to the device's floor (its last reset).
func (s *Service) sinceOf(id string, p Params) api.Since {
	since, _ := api.ParseSince(p.Since, time.Now())
	if floor := s.floorOf(id); since.Time.Before(floor) {
		since.Time = floor
	}
	return since
}

func (s *Service) logs(ctx context.Context, b backend.Backend, id string, p Params) (any, error) {
	lr, has := b.(backend.LogReader)
	if !has {
		return nil, missing(b, "logs")
	}
	since := s.sinceOf(id, p)
	q := api.LogQuery{App: p.App, Since: since.Time, After: since.Cursor, Level: api.LogLevel(p.Level), Contains: p.Contains, Regex: p.Regex}
	batch, err := lr.Logs(ctx, id, q)
	if err != nil {
		return nil, err
	}
	var re *regexp.Regexp
	if q.Regex != "" {
		re, _ = api.CompileLogRegex(q.Regex)
	}
	out := api.Logs{Partial: batch.Partial}
	out.Entries, out.Omitted = selectLogs(batch.Entries, q, re, p.Limit, p.MaxBytes)
	if batch.Cursor > 0 {
		out.Cursor = api.LogCursor(batch.Cursor)
	}
	if !batch.CapturedFrom.IsZero() {
		t := batch.CapturedFrom.UTC()
		out.CapturedFrom = &t
	}
	return out, nil
}

// selectLogs applies q's time, level and text filters to entries (oldest first) and keeps the
// newest matches within limit entries and maxBytes bytes (LogEntry.Size). The newest match is
// always kept, its message cut to fit. omitted counts the older matches left out.
func selectLogs(entries []api.LogEntry, q api.LogQuery, re *regexp.Regexp, limit, maxBytes int) (kept []api.LogEntry, omitted int) {
	minRank := q.Level.Rank()
	match := func(e *api.LogEntry) bool {
		if !q.Since.IsZero() && e.Time.Before(q.Since) {
			return false
		}
		if minRank > 0 && e.Level.Rank() < minRank {
			return false
		}
		if q.Contains != "" && !strings.Contains(strings.ToLower(e.Tag), q.Contains) && !strings.Contains(strings.ToLower(e.Message), q.Contains) {
			return false
		}
		if re != nil && !re.MatchString(e.Tag) && !re.MatchString(e.Message) {
			return false
		}
		return true
	}
	matched, size, full := 0, 0, false
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if !match(&e) {
			continue
		}
		matched++
		if full || len(kept) >= limit {
			continue
		}
		e.Time = e.Time.UTC()
		e.Message = api.CutText(e.Message, api.MaxLogMessage)
		if size+e.Size() > maxBytes {
			if len(kept) > 0 {
				full = true
				continue
			}
			e.Message = api.CutText(e.Message, max(0, maxBytes-(e.Size()-len(e.Message))))
		}
		size += e.Size()
		kept = append(kept, e)
	}
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	if kept == nil {
		kept = []api.LogEntry{}
	}
	return kept, matched - len(kept)
}

func (s *Service) crashes(ctx context.Context, b backend.Backend, id string, p Params) (any, error) {
	cr, has := b.(backend.CrashReader)
	if !has {
		return nil, missing(b, "crashes")
	}
	q := api.CrashQuery{App: p.App, Since: s.sinceOf(id, p).Time}
	list, err := cr.Crashes(ctx, id, q)
	if err != nil {
		return nil, err
	}
	out := api.Crashes{Crashes: []api.Crash{}}
	for _, c := range list {
		if (!q.Since.IsZero() && c.Time.Before(q.Since)) || (q.App != "" && c.App != q.App) {
			continue
		}
		c.Time = c.Time.UTC()
		out.Crashes = append(out.Crashes, c.Clamp(api.MaxCrashFrames))
	}
	sort.SliceStable(out.Crashes, func(i, j int) bool { return out.Crashes[i].Time.Before(out.Crashes[j].Time) })
	if n := len(out.Crashes); n > p.Limit {
		out.Omitted = n - p.Limit
		out.Crashes = out.Crashes[n-p.Limit:]
	}
	// The agent has now seen these: the notice does not repeat them.
	s.mu.Lock()
	st := s.stateLocked(id)
	for _, c := range out.Crashes {
		if c.App == st.watch && c.Time.After(st.reported) {
			st.reported = c.Time
		}
	}
	s.mu.Unlock()
	return out, nil
}

// appCrash asks the backend's watcher about the watched app: its newest crash at or after
// from (and its launch), or nil without a watcher, a watched app or a crash.
func (s *Service) appCrash(ctx context.Context, b backend.Backend, id string, from time.Time) *api.AppCrash {
	w, has := b.(backend.AppWatcher)
	if !has {
		return nil
	}
	s.mu.Lock()
	st := s.stateLocked(id)
	app, since := st.watch, st.since
	s.mu.Unlock()
	if app == "" {
		return nil
	}
	if from.Before(since) {
		from = since
	}
	wctx, cancel := context.WithTimeout(ctx, watchTimeout)
	state, err := w.AppState(wctx, id, app, from)
	cancel()
	if err != nil {
		log.Printf("app state of %s on %s: %v", app, id, err)
		return nil
	}
	if state.Crash == nil || state.Crash.Time.Before(from) {
		return nil
	}
	c := &api.AppCrash{Crash: state.Crash.Clamp(api.NoticeFrames), Running: state.Running}
	c.Time = c.Time.UTC()
	if c.App == "" {
		c.App = app
	}
	return c
}

// crashedUntold returns the watched app's crash when the app is down and the agent has not
// been told yet: the crash came during the call that started at start, or after the last
// call that reported one. Once told, the agent may be waiting for something else (it tapped
// the app's icon, say), so a wait runs its course.
func (s *Service) crashedUntold(ctx context.Context, b backend.Backend, id string, start time.Time) *api.AppCrash {
	c := s.appCrash(ctx, b, id, time.Time{})
	if c == nil || c.Running {
		return nil
	}
	s.mu.Lock()
	st := s.stateLocked(id)
	untold := c.App == st.watch && (!c.Time.Before(start) || c.Time.After(st.reported))
	s.mu.Unlock()
	if !untold {
		return nil
	}
	return c
}

// markReported records that the agent was told about the watched app's crash at t; it reports
// whether that crash was new to it.
func (s *Service) markReported(id string, c *api.AppCrash) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stateLocked(id)
	if c.App != st.watch || !c.Time.After(st.reported) {
		return false
	}
	st.reported = c.Time
	return true
}

// report adds the crash notice to a finished call on a device. A new crash of the watched app
// goes into the result once; an error while the app is down (or right after its crash) says
// why the call failed, with the code app_crashed.
func (s *Service) report(ctx context.Context, b backend.Backend, id string, res any, err error) (any, *api.AppCrash, error) {
	var told *api.AppCrashedError
	if errors.As(err, &told) { // a wait already stopped on the crash
		s.markReported(id, &told.Crash)
		return nil, &told.Crash, err
	}
	c := s.appCrash(ctx, b, id, time.Time{})
	if c == nil {
		return res, nil, err
	}
	isNew := s.markReported(id, c)
	if err != nil {
		if isNew || !c.Running {
			return nil, c, &api.AppCrashedError{Crash: *c, Err: err}
		}
		return nil, nil, err
	}
	if isNew {
		return res, c, nil
	}
	return res, nil, nil
}
