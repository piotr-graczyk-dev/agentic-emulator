package android

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
)

// encRecord encodes one record as `logcat -B` prints it (logger_entry v4, 28-byte header).
func encRecord(lid uint32, pid, tid, uid int, t time.Time, payload []byte) []byte {
	var b bytes.Buffer
	h := make([]byte, 28)
	binary.LittleEndian.PutUint16(h[0:], uint16(len(payload)))
	binary.LittleEndian.PutUint16(h[2:], 28)
	binary.LittleEndian.PutUint32(h[4:], uint32(int32(pid)))
	binary.LittleEndian.PutUint32(h[8:], uint32(tid))
	binary.LittleEndian.PutUint32(h[12:], uint32(t.Unix()))
	binary.LittleEndian.PutUint32(h[16:], uint32(t.Nanosecond()))
	binary.LittleEndian.PutUint32(h[20:], lid)
	binary.LittleEndian.PutUint32(h[24:], uint32(uid))
	b.Write(h)
	b.Write(payload)
	return b.Bytes()
}

// textPayload is a text record's payload: priority, tag NUL, message NUL.
func textPayload(prio byte, tag, msg string) []byte {
	return append(append([]byte{prio}, []byte(tag+"\x00"+msg)...), 0)
}

// eventPayload is an events record's payload: the tag number and a list of values.
func eventPayload(tag int32, vals ...any) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, tag)
	encEventValue(&b, vals)
	return b.Bytes()
}

func encEventValue(b *bytes.Buffer, v any) {
	switch x := v.(type) {
	case int:
		b.WriteByte(evInt)
		binary.Write(b, binary.LittleEndian, int32(x))
	case int64:
		b.WriteByte(evLong)
		binary.Write(b, binary.LittleEndian, x)
	case float32:
		b.WriteByte(evFloat)
		binary.Write(b, binary.LittleEndian, math.Float32bits(x))
	case string:
		b.WriteByte(evString)
		binary.Write(b, binary.LittleEndian, int32(len(x)))
		b.WriteString(x)
	case []any:
		b.WriteByte(evList)
		b.WriteByte(byte(len(x)))
		for _, e := range x {
			encEventValue(b, e)
		}
	}
}

func TestReadLogRecordText(t *testing.T) {
	at := time.Unix(1794034991, 635000123)
	data := encRecord(lidCrash, 5267, 5268, 10229, at, textPayload(6, "AndroidRuntime", "FATAL EXCEPTION: main\n\tat x\n"))
	rec, err := readLogRecord(bufio.NewReader(bytes.NewReader(data)))
	if err != nil {
		t.Fatal(err)
	}
	want := logRecord{lid: lidCrash, pid: 5267, tid: 5268, uid: 10229, time: at, prio: 6, tag: "AndroidRuntime", msg: "FATAL EXCEPTION: main\n\tat x"}
	if !reflect.DeepEqual(rec, want) {
		t.Fatalf("got %+v\nwant %+v", rec, want)
	}
}

func TestReadLogRecordCutMessageAndSequence(t *testing.T) {
	at := time.Unix(1794034991, 0)
	// logd cut the message: no closing NUL.
	cut := append([]byte{4}, []byte("Tag\x00abc")...)
	var data []byte
	data = append(data, encRecord(lidMain, 1, 2, 1000, at, cut)...)
	data = append(data, encRecord(lidSystem, 3, 4, 1000, at, textPayload(5, "T2", "second"))...)
	r := bufio.NewReader(bytes.NewReader(data))
	a, err := readLogRecord(r)
	if err != nil || a.tag != "Tag" || a.msg != "abc" || a.prio != 4 {
		t.Fatalf("first: %+v %v", a, err)
	}
	b, err := readLogRecord(r)
	if err != nil || b.tag != "T2" || b.msg != "second" || b.lid != lidSystem {
		t.Fatalf("second: %+v %v", b, err)
	}
	if _, err := readLogRecord(r); !errors.Is(err, io.EOF) {
		t.Fatalf("after the last record: %v, want EOF", err)
	}
}

func TestReadLogRecordHeaders(t *testing.T) {
	at := time.Unix(1794034991, 0)
	v4 := encRecord(lidMain, 7, 8, 10001, at, textPayload(4, "t", "m"))

	// hdr_size 24 (v3): no uid.
	v3 := append([]byte(nil), v4[:24]...)
	binary.LittleEndian.PutUint16(v3[2:], 24)
	v3 = append(v3, v4[28:]...)
	rec, err := readLogRecord(bufio.NewReader(bytes.NewReader(v3)))
	if err != nil || rec.uid != -1 || rec.pid != 7 || rec.msg != "m" {
		t.Fatalf("v3 header: %+v %v", rec, err)
	}

	// A larger header: its extra bytes are skipped.
	v5 := append([]byte(nil), v4[:28]...)
	binary.LittleEndian.PutUint16(v5[2:], 32)
	v5 = append(v5, 0, 0, 0, 0)
	v5 = append(v5, v4[28:]...)
	rec, err = readLogRecord(bufio.NewReader(bytes.NewReader(v5)))
	if err != nil || rec.uid != 10001 || rec.msg != "m" {
		t.Fatalf("32-byte header: %+v %v", rec, err)
	}

	// v1 (hdr_size 0) and nonsense sizes are refused rather than misread.
	for _, hs := range []uint16{0, 20, 200} {
		bad := append([]byte(nil), v4...)
		binary.LittleEndian.PutUint16(bad[2:], hs)
		if _, err := readLogRecord(bufio.NewReader(bytes.NewReader(bad))); err == nil {
			t.Errorf("hdr_size %d: no error", hs)
		}
	}

	// A record cut short is an error, not a short record.
	if _, err := readLogRecord(bufio.NewReader(bytes.NewReader(v4[:len(v4)-2]))); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("truncated record: %v, want ErrUnexpectedEOF", err)
	}
}

func TestReadLogRecordEvent(t *testing.T) {
	at := time.Unix(1794034991, 637000000)
	// am_crash as system_server logs it (the device's own values).
	p := eventPayload(evAmCrash, 5267, 0, "dev.agemu.probe", 816332356, "java.lang.IllegalStateException", "probe crash #2", "LabActivity.java", 152, 0)
	rec, err := readLogRecord(bufio.NewReader(bytes.NewReader(encRecord(lidEvents, 702, 702, 1000, at, p))))
	if err != nil {
		t.Fatal(err)
	}
	if rec.event != evAmCrash {
		t.Fatalf("event tag %d", rec.event)
	}
	if pid, ok := eventInt(rec.data, 0); !ok || pid != 5267 {
		t.Errorf("pid %v %v", pid, ok)
	}
	if exc, ok := eventStr(rec.data, 4); !ok || exc != "java.lang.IllegalStateException" {
		t.Errorf("exception %q %v", exc, ok)
	}
	if _, ok := eventStr(rec.data, 0); ok {
		t.Error("an int read as a string")
	}
	if _, ok := eventInt(rec.data, 9); ok {
		t.Error("a missing field read as present")
	}
}

func TestParseEventValueTypes(t *testing.T) {
	var b bytes.Buffer
	encEventValue(&b, []any{1, int64(1) << 40, float32(1.5), "s", []any{2, "t"}})
	v, n := parseEventValue(b.Bytes())
	if n != b.Len() {
		t.Fatalf("used %d of %d bytes", n, b.Len())
	}
	want := []any{int64(1), int64(1) << 40, float64(1.5), "s", []any{int64(2), "t"}}
	if !reflect.DeepEqual(v, want) {
		t.Fatalf("got %#v, want %#v", v, want)
	}
	// A string whose length runs past the payload keeps what is there.
	bad := []byte{evString, 100, 0, 0, 0, 'a', 'b'}
	if v, n := parseEventValue(bad); v != "ab" || n != len(bad) {
		t.Errorf("short string: %q %d", v, n)
	}
}

func TestLogLevel(t *testing.T) {
	want := map[byte]api.LogLevel{0: "", 1: "", 2: api.LogVerbose, 3: api.LogDebug, 4: api.LogInfo, 5: api.LogWarn, 6: api.LogError, 7: api.LogFatal, 8: ""}
	for p, l := range want {
		if got := logLevel(p); got != l {
			t.Errorf("priority %d: %q, want %q", p, got, l)
		}
	}
}

func TestGuestTimeArg(t *testing.T) {
	// Rounded down to the millisecond, so a record at the exact time is not left out.
	if got := guestTimeArg(time.Unix(1794034991, 635999999)); got != "1794034991.635" {
		t.Errorf("got %s", got)
	}
	if got := guestTimeArg(time.Unix(1794034991, 7000000)); got != "1794034991.007" {
		t.Errorf("got %s", got)
	}
}
