package openai

import (
	"encoding/json"
	"strings"
	"testing"
)

const tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFBQIAX8jx0gAAAABJRU5ErkJggg=="

func TestUserContentImages(t *testing.T) {
	raw := json.RawMessage(`[{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,` + tinyPNG + `"}}]`)
	text, imgs, err := UserContent(raw)
	if err != nil {
		t.Fatal(err)
	}
	if text != "what is this?" || len(imgs) != 1 || imgs[0].MimeType != "image/png" || imgs[0].Base64 != tinyPNG {
		t.Errorf("text=%q imgs=%+v", text, imgs)
	}
}

func TestUserContentRejectsBadImages(t *testing.T) {
	for _, url := range []string{
		"https://example.com/cat.png",          // remote URL: not fetched
		"data:image/png," + tinyPNG,            // not base64-flagged
		"data:image/svg+xml;base64," + tinyPNG, // type not allowed
		"data:image/png;base64,@@not-base64@@",
	} {
		raw := json.RawMessage(`[{"type":"image_url","image_url":{"url":"` + url + `"}}]`)
		if _, _, err := UserContent(raw); err == nil {
			t.Errorf("%s: expected error", url)
		}
	}
	if _, _, err := UserContent(json.RawMessage(`[{"type":"input_audio"}]`)); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("audio part: %v", err)
	}
}

func TestParseDataURLNormalises(t *testing.T) {
	// image/jpg is mapped to image/jpeg; unpadded base64 is re-padded.
	img, err := ParseDataURL("data:image/jpg;base64,QUJDRA")
	if err != nil {
		t.Fatal(err)
	}
	if img.MimeType != "image/jpeg" || img.Base64 != "QUJDRA==" {
		t.Errorf("got %+v", img)
	}
}
