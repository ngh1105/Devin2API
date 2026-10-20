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
// using the CLIProxyAPI method: sequentially compute sha256(seed+"-"+counter),
// append the hex digest to an accumulator, and truncate to 732 chars.
func GenerateFingerprint(seed string) string {
	var buf []byte
	counter := 0
	for len(buf) < 732 {
		in := make([]byte, 0, len(seed)+len(fmt.Sprintf("%d", counter))+1)
		in = append(in, seed...)
		in = append(in, '-')
		in = append(in, fmt.Sprintf("%d", counter)...)
		h := sha256.Sum256(in)
		buf = append(buf, hex.EncodeToString(h[:])...)
		counter++
	}
	return string(buf[:732])
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
	return PwFieldBytes(1, clientMetadata(sessionToken, fingerprint))
}

// clientMetadata encodes the ClientMetadata message shared by GetUserStatus
// and GetChatMessage (field layout documented on BuildGetUserStatusRequest).
func clientMetadata(sessionToken, fingerprint string) []byte {
	var inner []byte
	inner = append(inner, pwFieldString(1, "chisel")...)
	inner = append(inner, pwFieldString(2, "3000.10.21")...)
	inner = append(inner, pwFieldString(3, sessionToken)...)
	inner = append(inner, pwFieldString(4, "en")...)
	inner = append(inner, pwFieldString(5, runtime.GOOS)...)
	inner = append(inner, pwFieldString(7, "3000.10.21")...)
	inner = append(inner, pwFieldString(12, "chisel")...)
	inner = append(inner, pwFieldString(31, fingerprint)...)
	return inner
}

// ParseGetUserStatusResponse decodes the protobuf response from
// SeatManagementService.GetUserStatus into *store.Usage. A truncated
// message, at any level, is an error; fields of an unexpected wire type
// are skipped rather than read as zero values.
func ParseGetUserStatusResponse(data []byte) (*store.Usage, error) {
	usage := &store.Usage{}

	r := &pwReader{data: data}
	for {
		field, wire, val, _, ok := r.Next()
		if !ok {
			break
		}
		if field != 1 || wire != wireBytes {
			continue // skip unknown top-level fields
		}

		// val is the UserStatus bytes — parse them.
		sub := &pwReader{data: val}
		for {
			sf, wire, sval, _, ok := sub.Next()
			if !ok {
				break
			}
			if wire != wireBytes {
				continue
			}
			switch sf {
			case 3:
				usage.UserName = string(sval)
			case 5:
				usage.OrgID = string(sval)
			case 7:
				usage.Email = string(sval)
			case 13:
				if err := parsePlanStatus(sval, usage); err != nil {
					return nil, err
				}
			case 36:
				usage.UserID = string(sval)
			}
		}
		if err := sub.Done(); err != nil {
			return nil, fmt.Errorf("parse UserStatus: %w", err)
		}
	}
	if err := r.Done(); err != nil {
		return nil, fmt.Errorf("parse GetUserStatus: %w", err)
	}

	return usage, nil
}

func parsePlanStatus(data []byte, u *store.Usage) error {
	sub := &pwReader{data: data}
	for {
		sf, wire, sval, svarint, ok := sub.Next()
		if !ok {
			break
		}
		switch {
		case sf == 1 && wire == wireBytes: // PlanInfo
			pinfo := make([]byte, len(sval))
			copy(pinfo, sval)
			if err := parsePlanInfo(pinfo, u); err != nil {
				return err
			}
		case wire != wireVarint:
			continue
		case sf == 2: // plan_start (seconds, varint)
			u.PlanStart = time.Unix(int64(svarint), 0).UTC()
		case sf == 3: // plan_end (seconds, varint)
			u.PlanEnd = time.Unix(int64(svarint), 0).UTC()
		case sf == 14: // daily_quota_remaining_percent
			u.DailyRemainingPercent = int64(svarint)
		case sf == 15: // weekly_quota_remaining_percent
			u.WeeklyRemainingPercent = int64(svarint)
		case sf == 17: // daily_quota_reset_at (seconds)
			u.DailyResetAt = time.Unix(int64(svarint), 0).UTC()
		case sf == 18: // weekly_quota_reset_at (seconds)
			u.WeeklyResetAt = time.Unix(int64(svarint), 0).UTC()
		}
	}
	if err := sub.Done(); err != nil {
		return fmt.Errorf("parse PlanStatus: %w", err)
	}
	return nil
}

func parsePlanInfo(data []byte, u *store.Usage) error {
	sub := &pwReader{data: data}
	for {
		sf, wire, sval, _, ok := sub.Next()
		if !ok {
			break
		}
		if wire != wireBytes {
			continue
		}
		switch sf {
		case 2:
			u.Plan = string(sval)
		case 33: // OrgInfo embedded in PlanInfo
			orgInfo := make([]byte, len(sval))
			copy(orgInfo, sval)
			if err := parseOrgInfo(orgInfo, u); err != nil {
				return err
			}
		}
	}
	if err := sub.Done(); err != nil {
		return fmt.Errorf("parse PlanInfo: %w", err)
	}
	return nil
}

func parseOrgInfo(data []byte, u *store.Usage) error {
	sub := &pwReader{data: data}
	for {
		sf, wire, sval, _, ok := sub.Next()
		if !ok {
			break
		}
		if wire != wireBytes {
			continue
		}
		switch sf {
		case 4:
			u.OrgID = string(sval)
		case 8:
			u.OrgName = string(sval)
		}
	}
	if err := sub.Done(); err != nil {
		return fmt.Errorf("parse OrgInfo: %w", err)
	}
	return nil
}

// FetchUserStatus POSTs to <serverBase>/exa.seat_management_pb.SeatManagementService/GetUserStatus
// with Content-Type application/proto and returns parsed Usage. On HTTP 401 it
// returns ErrTokenExpired.
func FetchUserStatus(ctx context.Context, serverBase, sessionToken string) (*store.Usage, error) {
	if serverBase == "" {
		serverBase = ServerBaseURL
	}

	reqData := BuildGetUserStatusRequest(sessionToken, Fingerprint(sessionToken))

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		serverBase+"/exa.seat_management_pb.SeatManagementService/GetUserStatus",
		bytes.NewReader(reqData),
	)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/proto")
	httpReq.Header.Set("Authorization", "Bearer "+sessionToken)

	resp, err := apiHTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("user status request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		io.Copy(io.Discard, io.LimitReader(resp.Body, maxJSONBytes))
		return nil, ErrTokenExpired
	}

	if resp.StatusCode != http.StatusOK {
		// Keep the Connect error (quota, permission_denied...) so callers
		// can tell a 429 from a 5xx.
		return nil, fmt.Errorf("user status: %w", httpStatusError(resp))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBytes))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	usage, err := ParseGetUserStatusResponse(body)
	if err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return usage, nil
}
