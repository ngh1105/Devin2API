package devin

import "testing"

// TestReaderRejectsHugeLength: a length varint above MaxInt64 used to wrap
// negative, pass the bounds check and panic when slicing. It must be
// reported as truncation instead, for every parser built on the reader.
func TestReaderRejectsHugeLength(t *testing.T) {
	bad := []byte{0x1a, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}
	r := &pwReader{data: bad}
	if _, _, _, _, ok := r.Next(); ok {
		t.Fatal("huge length accepted")
	}
	if r.Done() == nil {
		t.Error("truncation not reported")
	}
	// The real parsers must not panic either.
	ParseModels(bad)
	ParseGetUserStatusResponse(bad)
	parseChatFrame(bad, &ChatResult{})
}

// TestToolTrackerDropKeepsLastCall: id-less fragments of a dropped call
// (over maxToolCalls) must not be appended to the last accepted call.
func TestToolTrackerDropKeepsLastCall(t *testing.T) {
	tr := newToolTracker()
	for i := 0; i < maxToolCalls; i++ {
		tr.feed(rawToolDelta{id: string(rune(0x4e00 + i)), name: "f", args: "{}"})
	}
	tr.feed(rawToolDelta{id: "overflow", name: "g", args: `{"a":`})
	tr.feed(rawToolDelta{args: `1}`})
	if got := tr.calls[maxToolCalls-1].Arguments; got != "{}" {
		t.Errorf("last accepted call corrupted: %q", got)
	}
	if len(tr.calls) != maxToolCalls {
		t.Errorf("calls = %d", len(tr.calls))
	}
}
