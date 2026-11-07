package android

// Reading logd's records as `logcat -B` (binary) prints them: each record is the logger_entry
// header logd sends its readers, then the payload. Binary keeps a record whole (a stack trace
// logged in one call stays one record) and carries the writer's uid, which text formats lose.

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
)

// logd's buffers (log_id_t).
const (
	lidMain   = 0
	lidEvents = 2
	lidSystem = 3
	lidCrash  = 4
)

// logcatBuffers are the buffers agemu reads: the text buffers apps and the system write to,
// and the events buffer, where ActivityManager records process starts, deaths, crashes and
// ANRs (the watcher's input; its records are not log lines).
const logcatBuffers = "main,system,crash,events"

// logRecord is one record of logd, with the guest's timestamp.
type logRecord struct {
	lid      uint32
	pid, tid int
	uid      int // -1 when the header has none
	time     time.Time
	// Text buffers: the priority, the tag and the message (trailing newlines removed).
	prio byte
	tag  string
	msg  string
	// Events buffer: the event tag number and its values (int32, int64, float32, string, []any).
	event int32
	data  any
}

// maxHeader bounds logger_entry's hdr_size: today's header is 28 bytes; a larger one is
// read and its extra fields skipped.
const maxHeader = 128

// readLogRecord reads one binary record. The header (little-endian, packed):
//
//	uint16 len; uint16 hdr_size; int32 pid; uint32 tid; uint32 sec; uint32 nsec; uint32 lid; uint32 uid
//
// hdr_size 24 (v2/v3) ends before uid; v1 headers (hdr_size 0, 20 bytes, no buffer id) are
// refused, as no supported Android image writes them.
func readLogRecord(r *bufio.Reader) (logRecord, error) {
	var head [4]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return logRecord{}, err
	}
	n := int(binary.LittleEndian.Uint16(head[0:]))
	hs := int(binary.LittleEndian.Uint16(head[2:]))
	if hs < 24 || hs > maxHeader {
		return logRecord{}, fmt.Errorf("logcat: unsupported record header size %d", hs)
	}
	buf := make([]byte, hs-4+n)
	if _, err := io.ReadFull(r, buf); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return logRecord{}, err
	}
	h := buf[:hs-4]
	rec := logRecord{
		pid: int(int32(binary.LittleEndian.Uint32(h[0:]))),
		tid: int(binary.LittleEndian.Uint32(h[4:])),
		lid: binary.LittleEndian.Uint32(h[16:]),
		uid: -1,
	}
	sec, nsec := binary.LittleEndian.Uint32(h[8:]), binary.LittleEndian.Uint32(h[12:])
	rec.time = time.Unix(int64(sec), int64(nsec))
	if hs >= 28 {
		rec.uid = int(binary.LittleEndian.Uint32(h[20:]))
	}
	payload := buf[hs-4:]
	if rec.lid == lidEvents {
		if len(payload) < 4 {
			return rec, nil
		}
		rec.event = int32(binary.LittleEndian.Uint32(payload))
		rec.data, _ = parseEventValue(payload[4:])
		return rec, nil
	}
	// Text: priority byte, tag NUL, message NUL (the last NUL is missing when logd cut it).
	if len(payload) == 0 {
		return rec, nil
	}
	rec.prio = payload[0]
	rest := payload[1:]
	tag, msg, _ := strings.Cut(string(rest), "\x00")
	rec.tag = tag
	msg, _, _ = strings.Cut(msg, "\x00")
	rec.msg = strings.TrimRight(msg, "\n")
	return rec, nil
}

// Event value types (android/log.h AndroidEventLogType).
const (
	evInt    = 0
	evLong   = 1
	evString = 2
	evList   = 3
	evFloat  = 4
)

// parseEventValue decodes one typed event value and returns it with the bytes it used.
func parseEventValue(b []byte) (any, int) {
	if len(b) < 1 {
		return nil, 0
	}
	switch b[0] {
	case evInt:
		if len(b) < 5 {
			return nil, len(b)
		}
		return int64(int32(binary.LittleEndian.Uint32(b[1:]))), 5
	case evLong:
		if len(b) < 9 {
			return nil, len(b)
		}
		return int64(binary.LittleEndian.Uint64(b[1:])), 9
	case evFloat:
		if len(b) < 5 {
			return nil, len(b)
		}
		return float64(math.Float32frombits(binary.LittleEndian.Uint32(b[1:]))), 5
	case evString:
		if len(b) < 5 {
			return nil, len(b)
		}
		n := int(binary.LittleEndian.Uint32(b[1:]))
		if n < 0 || 5+n > len(b) {
			return string(b[5:]), len(b)
		}
		return string(b[5 : 5+n]), 5 + n
	case evList:
		if len(b) < 2 {
			return nil, len(b)
		}
		count, used := int(b[1]), 2
		list := make([]any, 0, count)
		for i := 0; i < count && used < len(b); i++ {
			v, n := parseEventValue(b[used:])
			if n == 0 {
				break
			}
			list = append(list, v)
			used += n
		}
		return list, used
	}
	return nil, len(b)
}

// eventInt and eventStr read item i of an event's list; ok is false when it is missing or of
// another type (an event's format can gain fields across Android versions, never lose them).
func eventInt(data any, i int) (int64, bool) {
	l, _ := data.([]any)
	if i >= len(l) {
		return 0, false
	}
	v, ok := l[i].(int64)
	return v, ok
}

func eventStr(data any, i int) (string, bool) {
	l, _ := data.([]any)
	if i >= len(l) {
		return "", false
	}
	v, ok := l[i].(string)
	return v, ok
}

// logLevel maps an Android log priority to the neutral level; 0 and 1 (unknown, default) and
// 8 (silent) have none.
func logLevel(prio byte) api.LogLevel {
	switch prio {
	case 2:
		return api.LogVerbose
	case 3:
		return api.LogDebug
	case 4:
		return api.LogInfo
	case 5:
		return api.LogWarn
	case 6:
		return api.LogError
	case 7:
		return api.LogFatal
	}
	return ""
}

// guestTimeArg formats a guest time for logcat -T ("<seconds>.<milliseconds>"), rounded down to
// the millisecond so that no record at or after t is left out.
func guestTimeArg(t time.Time) string {
	ms := t.UnixMilli()
	return fmt.Sprintf("%d.%03d", ms/1000, ms%1000)
}
