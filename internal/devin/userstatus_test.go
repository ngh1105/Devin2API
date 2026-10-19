package devin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
)

func TestProtowireTagMultiByte(t *testing.T) {
	// Field 31 >= 16 → multi-byte varint tag
	got := pwTag(31, wireBytes)
	want := []byte{0xFA, 0x01}
	if !bytes.Equal(got, want) {
		t.Errorf("pwTag(31, %d) = %v; want %v", wireBytes, got, want)
	}

	// Field 36 should also be multi-byte: (36<<3)|2 = 290 → [0xA2, 0x02]
	got36 := pwTag(36, wireBytes)
	want36 := []byte{0xA2, 0x02}
	if len(got36) != 2 || got36[0] != want36[0] || got36[1] != want36[1] {
		t.Errorf("pwTag(36, %d) = %v; want %v", wireBytes, got36, want36)
	}

	// Field 14 should be single byte: (14<<3)|2 = 114 = 0x72
	got14 := pwTag(14, wireBytes)
	if len(got14) != 1 || got14[0] != 0x72 {
		t.Errorf("pwTag(14, %d) = %v; want 0x72", wireBytes, got14)
	}
}

// ---------------------------------------------------------------------------
// Golden-test: build expected request independently
// ---------------------------------------------------------------------------

func TestBuildGetUserStatusRequest(t *testing.T) {
	token := "test-session-token"
	fp := GenerateFingerprint(seedForTest)

	expected := encodeClientMetadata(token, fp)
	got := BuildGetUserStatusRequest(token, fp)
	if !bytes.Equal(got, expected) {
		t.Errorf("request length: got %d, want %d\n"+
			"expected (%d): % x\n"+
			"got      (%d): % x",
			len(got), len(expected), len(expected), expected, len(got), got)
	}
}

// Independent tiny protowire encoder used only by the golden test.
func encodeClientMetadata(token, fingerprint string) []byte {
	type fstr struct {
		field int
		val   string
	}
	fields := []fstr{
		{1, "chisel"},
		{2, "3000.10.21"},
		{3, token},
		{4, "en"},
		{5, runtime.GOOS},
		{7, "3000.10.21"},
		{12, "chisel"},
		{31, fingerprint},
	}

	var inner []byte
	for _, f := range fields {
		inner = append(inner, pwString(f.field, f.val)...) // same as pwFieldString
	}

	outer := pwTag(1, wireBytes)
	outer = append(outer, pwVarint(uint64(len(inner)))...)
	outer = append(outer, inner...)
	return outer
}

// ---------------------------------------------------------------------------
// Parse test
// ---------------------------------------------------------------------------

func TestParseGetUserStatusResponse(t *testing.T) {
	response := buildResponseFixture()

	usage, err := ParseGetUserStatusResponse(response)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		got  interface{}
		want interface{}
	}{
		{"UserName", usage.UserName, "Test User"},
		{"UserID", usage.UserID, "user-999"},
		{"Email", usage.Email, "test@example.com"},
		{"OrgID", usage.OrgID, "org-planid"},
		{"OrgName", usage.OrgName, "Test Org"},
		{"Plan", usage.Plan, "Pro"},
		{"DailyRemainingPercent", usage.DailyRemainingPercent, int64(42)},
		{"WeeklyRemainingPercent", usage.WeeklyRemainingPercent, int64(77)},
	}

	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("%s = %v; want %v", tc.name, tc.got, tc.want)
		}
	}
}

func buildResponseFixture() []byte {
	// Build PlanInfo: field 2="Pro", field 33=OrgInfo{4:"org-planid",8:"Test Org"}
	var pi []byte
	pi = append(pi, pwString(2, "Pro")...)
	// For field 33 of PlanInfo (multi-byte tag), embed OrgInfo as raw bytes
	orgInner := pwFieldString(4, "org-planid") // tag + len("org-planid") + org-planid
	orgInner = append(orgInner, pwFieldString(8, "Test Org")...)
	// Field 33 is multi-byte: (33<<3)|2 = 0xFA2 → varint [0x82, 0x01]
	pi = append(pi, pwTag(33, wireBytes)...)
	pi = append(pi, pwVarint(uint64(len(orgInner)))...)
	pi = append(pi, orgInner...)

	// Build PlanStatus: field 1=PlanInfo(bytes), field 14=42, field 15=77
	ps := pwString(1, string(pi))
	ps = append(ps, pwFieldVarint(14, 42)...)
	ps = append(ps, pwFieldVarint(15, 77)...)

	// UserStatus inner: flat fields
	var inner []byte
	inner = append(inner, pwString(3, "Test User")...)
	inner = append(inner, pwString(5, "team-abc")...)
	inner = append(inner, pwString(7, "test@example.com")...)
	// Wrap ps in bytes for field 13
	inner = append(inner, pwString(13, string(ps))...)
	inner = append(inner, pwString(36, "user-999")...)

	// Outer envelope: wrap all as field 1 (bytes)
	out := pwTag(1, wireBytes)
	out = append(out, pwVarint(uint64(len(inner)))...)
	out = append(out, inner...)

	return out
}

// ---------------------------------------------------------------------------
// 401 test
// ---------------------------------------------------------------------------

func TestUserStatus401(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
	}))
	defer ts.Close()

	_, err := FetchUserStatus(context.Background(), ts.URL, "dead-token")
	if err == nil {
		t.Fatal("expected error for 401")
	}
	if !errors.Is(err, ErrTokenExpired) {
		t.Errorf("error = %v; want ErrTokenExpired", err)
	}
}

const seedForTest = "devin2api-cli-testseed"
