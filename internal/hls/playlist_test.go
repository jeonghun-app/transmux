package hls

import (
	"strings"
	"testing"
	"time"
)

// A real ffmpeg hls muxer playlist, including the tag order it actually uses.
const ffmpegPlaylist = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:5
#EXT-X-MEDIA-SEQUENCE:12
#EXTINF:5.000000,
#EXT-X-PROGRAM-DATE-TIME:2026-08-31T17:00:00.000+0000
seg-000012.ts
#EXTINF:4.960000,
#EXT-X-PROGRAM-DATE-TIME:2026-08-31T17:00:05.000+0000
seg-000013.ts
#EXT-X-DISCONTINUITY
#EXTINF:9.920000,
seg-000014.ts
`

func TestParseLocal(t *testing.T) {
	segs, err := ParseLocal([]byte(ffmpegPlaylist))
	if err != nil {
		t.Fatalf("ParseLocal: %v", err)
	}
	if len(segs) != 3 {
		t.Fatalf("want 3 segments, got %d", len(segs))
	}
	if segs[0].Name != "seg-000012.ts" {
		t.Errorf("name = %q", segs[0].Name)
	}
	if segs[0].Duration != 5*time.Second {
		t.Errorf("duration = %v", segs[0].Duration)
	}
	if segs[0].ProgramDateTime.IsZero() {
		t.Error("expected a program date time on the first segment")
	}
	if got := segs[1].Duration; got != 4960*time.Millisecond {
		t.Errorf("second duration = %v", got)
	}
	// A long-GOP camera legitimately produces a segment longer than the
	// configured target; the parser must report the real value.
	if got := segs[2].Duration; got != 9920*time.Millisecond {
		t.Errorf("third duration = %v", got)
	}
	if !segs[2].Discontinuity {
		t.Error("expected discontinuity on the third segment")
	}
	if segs[0].Discontinuity || segs[1].Discontinuity {
		t.Error("discontinuity leaked onto an earlier segment")
	}
}

func TestParseLocalRejectsNonPlaylist(t *testing.T) {
	if _, err := ParseLocal([]byte("not a playlist\n")); err == nil {
		t.Fatal("expected an error for input without #EXTM3U")
	}
}

func TestParseLocalTornRead(t *testing.T) {
	// ffmpeg rewrites the playlist in place, so a scan can observe a
	// truncated file. Parsing must not invent a segment with no EXTINF.
	torn := "#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:5.000000,\n"
	segs, err := ParseLocal([]byte(torn))
	if err != nil {
		t.Fatalf("ParseLocal: %v", err)
	}
	if len(segs) != 0 {
		t.Fatalf("want 0 complete segments, got %d", len(segs))
	}
}

func TestRenderLiveTargetDurationUsesActualMax(t *testing.T) {
	pdt := time.Date(2026, 8, 31, 17, 0, 0, 0, time.UTC)
	segs := []PublishedSegment{
		{Sequence: 100, URI: "2026/08/31/seg-a.ts", Duration: 5 * time.Second, ProgramDateTime: pdt},
		// A 9.92 s segment must round the target up to 10, not stay at 5.
		{Sequence: 101, URI: "2026/08/31/seg-b.ts", Duration: 9920 * time.Millisecond},
	}
	out := string(RenderLive(segs, 3, "write-1"))

	for _, want := range []string{
		"#EXTM3U",
		"#EXT-X-VERSION:3",
		"#EXT-X-TARGETDURATION:10",
		"#EXT-X-MEDIA-SEQUENCE:100",
		"#EXT-X-DISCONTINUITY-SEQUENCE:3",
		"#EXTINF:5.000,",
		"#EXTINF:9.920,",
		"2026/08/31/seg-a.ts",
		"#EXT-X-PROGRAM-DATE-TIME:2026-08-31T17:00:00.000Z",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered playlist missing %q\n---\n%s", want, out)
		}
	}
	// A live playlist must not be marked complete.
	if strings.Contains(out, "#EXT-X-ENDLIST") {
		t.Error("live playlist must not contain EXT-X-ENDLIST")
	}
}

func TestRenderLiveEmitsDiscontinuityBeforeSegment(t *testing.T) {
	segs := []PublishedSegment{
		{Sequence: 1, URI: "a.ts", Duration: 5 * time.Second},
		{Sequence: 2, URI: "b.ts", Duration: 5 * time.Second, Discontinuity: true},
	}
	out := string(RenderLive(segs, 0, "write-1"))
	// Search for the exact tag line: a substring search for
	// "#EXT-X-DISCONTINUITY" would also match the header's
	// EXT-X-DISCONTINUITY-SEQUENCE tag.
	di := strings.Index(out, "#EXT-X-DISCONTINUITY\n")
	bi := strings.Index(out, "b.ts")
	ai := strings.Index(out, "a.ts")
	if di < 0 || bi < 0 {
		t.Fatalf("missing tags:\n%s", out)
	}
	if !(ai < di && di < bi) {
		t.Errorf("discontinuity must sit between a.ts and b.ts\n%s", out)
	}
}

func TestRenderLiveEmptyWindow(t *testing.T) {
	out := string(RenderLive(nil, 0, "write-1"))
	if !strings.Contains(out, "#EXT-X-TARGETDURATION:1") {
		t.Errorf("empty window must still emit a valid target duration:\n%s", out)
	}
	if !strings.Contains(out, "#EXT-X-MEDIA-SEQUENCE:0") {
		t.Errorf("empty window media sequence:\n%s", out)
	}
}
