package devin

import (
	"bytes"
	"testing"
)

func TestPWTagSingleByte(t *testing.T) {
	got := pwTag(1, wireBytes)
	want := []byte{0x0A}
	if !bytes.Equal(got, want) {
		t.Errorf("pwTag(1, %d) = %v; want %v", wireBytes, got, want)
	}
}

func TestPWTagMultiByte(t *testing.T) {
	got := pwTag(31, wireBytes)
	want := []byte{0xFA, 0x01}
	if len(got) != len(want) || !bytes.Equal(got, want) {
		t.Errorf("pwTag(31, 2) = %v; want %v", got, want)
	}

	got36 := pwTag(36, wireBytes)
	want36 := []byte{0xA2, 0x02}
	if len(got36) != 2 || !bytes.Equal(got36, want36) {
		t.Errorf("pwTag(36, 2) = %v; want %v", got36, want36)
	}

	got14 := pwTag(14, wireBytes)
	if len(got14) != 1 || got14[0] != 0x72 {
		t.Errorf("pwTag(14, 2) = %v; want 0x72", got14)
	}
}

func TestPWFieldVarint(t *testing.T) {
	got := pwFieldVarint(1, 42)
	want := []byte{0x08, 0x2A}
	checkBytes(t, "pwFieldVarint(1, 42)", got, want)
}

func TestPWReaderBasics(t *testing.T) {
	// Build using our own helpers: pwString(1, "hi") + pwFieldVarint(5, 42)
	data := append(pwString(1, "hi"), pwFieldVarint(5, 42)...)

	r := &pwReader{data: data}

	f1, w1, v1, vi1, ok := r.Next()
	if !ok {
		t.Fatal("expected first field")
	}
	if f1 != 1 {
		t.Errorf("field 1 = %d; want 1", f1)
	}
	if w1 != wireBytes {
		t.Errorf("wire 1 = %d; want %d", w1, wireBytes)
	}
	if string(v1) != "hi" {
		t.Errorf("val 1 = %q; want %q", v1, "hi")
	}
	if vi1 != 0 {
		t.Errorf("varint 1 = %d; want 0", vi1)
	}

	f2, w2, v2, vi2, ok := r.Next()
	if !ok {
		t.Fatal("expected second field")
	}
	if f2 != 5 {
		t.Errorf("field 2 = %d; want 5", f2)
	}
	if w2 != wireVarint {
		t.Errorf("wire 2 = %d; want %d", w2, wireVarint)
	}
	if vi2 != 42 {
		t.Errorf("varint 2 = %d; want 42", vi2)
	}
	_ = v2

	// No more fields
	if _, _, _, _, ok := r.Next(); ok {
		t.Error("expected no more fields")
	}
}

func TestPWReaderTruncated(t *testing.T) {
	r := &pwReader{data: []byte{0x0A}}
	if _, _, _, _, ok := r.Next(); ok {
		t.Fatal("expected truncated")
	}
}

func checkBytes(t *testing.T, name string, got, want []byte) {
	t.Helper()
	if !bytes.Equal(got, want) {
		t.Errorf("%s: got %v; want %v", name, got, want)
	}
}
