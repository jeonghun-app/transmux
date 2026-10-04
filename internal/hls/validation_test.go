package hls

import (
	"strings"
	"testing"
)

func TestParsersRejectInvalidSegmentDurations(t *testing.T) {
	for _, duration := range []string{"NaN", "Inf", "+Inf", "-1", "0", "1e30", "9223372036.854776"} {
		t.Run(duration, func(t *testing.T) {
			head := "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:" + duration + ",\n"
			if _, err := ParseLocal([]byte(head + "seg-000001.ts\n")); err == nil {
				t.Error("local parser accepted an invalid duration")
			}
			if _, err := ParsePublished([]byte(head + "2026/09/12/seg-000000001-1.ts\n")); err == nil {
				t.Error("recovery parser accepted an invalid duration")
			}
		})
	}
}

func TestPublishedParserRejectsAmbiguousRecoveryState(t *testing.T) {
	valid := "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:4.000,\n2026/09/12/seg-000000001-1.ts\n"
	for name, body := range map[string]string{
		"truncated":       valid + "#EXTINF:4.000,\n",
		"URI without inf": valid + "2026/09/12/seg-000000002-2.ts\n",
		"duplicate seq":   valid + "#EXT-X-MEDIA-SEQUENCE:9\n",
		"late seq":        strings.Replace(valid, "#EXT-X-MEDIA-SEQUENCE:1\n", "", 1) + "#EXT-X-MEDIA-SEQUENCE:99\n",
		"encrypted":       strings.Replace(valid, "#EXTINF:", "#EXT-X-KEY:METHOD=AES-128,URI=\"key\"\n#EXTINF:", 1),
		"byte range":      strings.Replace(valid, "#EXTINF:", "#EXT-X-BYTERANGE:100@0\n#EXTINF:", 1),
		"floor rewind":    valid + LastSequenceComment + "0\n",
		"duplicate id":    valid + WriteIDComment + "one\n" + WriteIDComment + "two\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParsePublished([]byte(body)); err == nil {
				t.Fatal("unsafe recovery manifest accepted")
			}
		})
	}
}
