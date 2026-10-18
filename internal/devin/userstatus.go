package devin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"time"

	"github.com/ngh1105/Devin2API/internal/store"
)

// ErrTokenExpired is returned when GetUserStatus returns HTTP 401.
var ErrTokenExpired = errors.New("token expired or revoked")

// GenerateFingerprint produces a deterministic 732-hex-char device fingerprint
// using a SHA-256 chain: seed → seed-0 → seed-1 → ... → seed-255 (256 inputs).
func GenerateFingerprint(seed string) string {
	h := sha256.Sum256([]byte(seed))
	for i := 0; i < 256; i++ {
		in := append(h[:], byte(i))
		h = sha256.Sum256(in)
	}
	return hex.EncodeToString(h[:])
}

// BuildGetUserStatusRequest builds the protobuf bytes for the ConnectRPC
// SeatManagementService.GetUserStatus call. Inner ClientMetadata fields:
//
//	field 1  = "chisel"          (wire 2 — bytes)
//	field 2  = "3000.10.21"      (wire 2)
//	field 3  = sessionToken      (wire 2)
//	field 4  = "en"              (wire 2)
//	field 5  = runtime.GOOS      (wire 2)
//	field 7  = "3000.10.21"      (wire 2)
//	field 12 = "chisel"          (wire 2)
//	field 31 = fingerprint       (wire 2 — multi-byte tag since ≥16)
//
// Outer envelope wraps the inner as field 1 (wire 2).
func BuildGetUserStatusRequest(sessionToken, fingerprint string) []byte {
	var inner []byte
	inner = append(inner, pwFieldString(1, "chisel")...)
	inner = append(inner, pwFieldString(2, "3000.10.21")...)
	inner = append(inner, pwFieldString(3, sessionToken)...)
	inner = append(inner, pwFieldString(4, "en")...)
	inner = append(inner, pwFieldString(5, runtime.GOOS)...)
	inner = append(inner, pwFieldString(7, "3000.10.21")...)
	inner = append(inner, pwFieldString(12, "chisel")...)
	inner = append(inner, pwFieldString(31, fingerprint)...)

	outer := pwTag(1, wireBytes)
	outer = append(outer, pwVarint(uint64(len(inner)))...)
	outer = append(outer, inner...)
	return outer
}

// ParseGetUserStatusResponse decodes the protobuf response from
// SeatManagementService.GetUserStatus into *store.Usage. Returns nil usage
// (nil pointer) on malformed input rather than panicking.
func ParseGetUserStatusResponse(data []byte) (*store.Usage, error) {
	usage := &store.Usage{}

	r := &pwReader{data: data}
	for {
		field, _, val, _, ok := r.Next()
		if !ok {
			break
		}
		if field != 1 {
			continue // skip unknown top-level fields
		}

		// val is the UserStatus bytes — parse them.
		sub := &pwReader{data: val}
		for {
			sf, _, sval, _, ok := sub.Next()
			if !ok {
				break
			}
			switch sf {
			case 3:
				usage.UserName = string(sval)
			case 5:
				usage.OrgID = string(sval)
			case 7:
				usage.Email = string(sval)
			case 13:
				parsePlanStatus(sval, usage)
			case 36:
				usage.UserID = string(sval)
			}
		}
		if err := sub.Done(); err != nil {
			return nil, fmt.Errorf("parse UserStatus: %w", err)
		}
	}

	return usage, nil
}

func parsePlanStatus(data []byte, u *store.Usage) {
	sub := &pwReader{data: data}
	for {
		sf, _, sval, svarint, ok := sub.Next()
		if !ok {
			break
		}
		switch sf {
		case 1: // PlanInfo
			parsePlanInfo(sval, u)
		case 2: // plan_start (subfield 1 = varint sec)
			u.PlanStart = time.Unix(int64(svarint), 0).UTC()
		case 3: // plan_end (subfield 1 = varint sec)
			u.PlanEnd = time.Unix(int64(svarint), 0).UTC()
		case 14:
			u.DailyRemainingPercent = int64(svarint)
		case 15:
			u.WeeklyRemainingPercent = int64(svarint)
		case 17:
			u.DailyResetAt = time.Unix(int64(svarint), 0).UTC()
		case 18:
			u.WeeklyResetAt = time.Unix(int64(svarint), 0).UTC()
		}
	}
}

func parsePlanInfo(data []byte, u *store.Usage) {
	sub := &pwReader{data: data}
	for {
		sf, _, sval, _, ok := sub.Next()
		if !ok {
			break
		}
		switch sf {
		case 2:
			u.Plan = string(sval)
		case 33:
			parseOrgInfo(sval, u)
		}
	}
}

func parseOrgInfo(data []byte, u *store.Usage) {
	sub := &pwReader{data: data}
	for {
		sf, _, sval, _, ok := sub.Next()
		if !ok {
			break
		}
		switch sf {
		case 4:
			u.OrgID = string(sval)
		case 8:
			u.OrgName = string(sval)
		}
	}
}

// FetchUserStatus POSTs to <serverBase>/exa.seat_management_pb.SeatManagementService/GetUserStatus
// with Content-Type application/proto and returns parsed Usage. On HTTP 401 it
// returns ErrTokenExpired.
func FetchUserStatus(ctx context.Context, serverBase, sessionToken string) (*store.Usage, error) {
	if serverBase == "" {
		serverBase = ServerBaseURL
	}

	fingerprint := GenerateFingerprint("devin2api-cli")
	reqData := BuildGetUserStatusRequest(sessionToken, fingerprint)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		serverBase+"/exa.seat_management_pb.SeatManagementService/GetUserStatus",
		bytes.NewReader(reqData),
	)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/proto")
	httpReq.Header.Set("Authorization", "Bearer "+sessionToken)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("user status request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		io.Copy(io.Discard, resp.Body)
		return nil, ErrTokenExpired
	}

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("user status: status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	usage, err := ParseGetUserStatusResponse(body)
	if err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return usage, nil
}
