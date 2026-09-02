package hls

import (
	"strings"
	"testing"
	"time"
)

func TestParsePublishedRecoversSequence(t *testing.T) {
	// Exactly the shape RenderLive produces.
	manifest := strings.Join([]string{
		"#EXTM3U",
		"#EXT-X-VERSION:3",
		"#EXT-X-TARGETDURATION:4",
		"#EXT-X-MEDIA-SEQUENCE:25",
		"#EXT-X-DISCONTINUITY-SEQUENCE:3",
		"#EXTINF:4.000,",
		"#EXT-X-PROGRAM-DATE-TIME:2026-08-31T17:43:07.331Z",
		"2026/08/31/seg-000000025-1788198187331.ts",
		"#EXT-X-DISCONTINUITY",
		"#EXTINF:8.000,",
		"#EXT-X-PROGRAM-DATE-TIME:2026-08-31T17:43:11.331Z",
		"2026/08/31/seg-000000026-1788198191331.ts",
		"",
	}, "\n")

	pub, err := ParsePublished([]byte(manifest))
	if err != nil {
		t.Fatalf("ParsePublished: %v", err)
	}
	if pub.MaxSequence != 26 {
		t.Errorf("MaxSequence = %d, want 26", pub.MaxSequence)
	}
	if pub.DiscontinuitySequence != 3 {
		t.Errorf("DiscontinuitySequence = %d, want 3", pub.DiscontinuitySequence)
	}
	if len(pub.Window) != 2 {
		t.Fatalf("window has %d entries, want 2", len(pub.Window))
	}
	if pub.Window[0].Sequence != 25 || pub.Window[1].Sequence != 26 {
		t.Errorf("window sequences = %d,%d", pub.Window[0].Sequence, pub.Window[1].Sequence)
	}
	if pub.Window[1].Duration != 8*time.Second {
		t.Errorf("second duration = %v, want 8s", pub.Window[1].Duration)
	}
	if !pub.Window[1].Discontinuity {
		t.Error("discontinuity was not recovered")
	}
	if pub.Window[0].Discontinuity {
		t.Error("discontinuity leaked onto the first segment")
	}
	if pub.Window[0].ProgramDateTime.IsZero() {
		t.Error("program date time was not recovered")
	}
}

// RenderLive and ParsePublished must be exact inverses for the fields that
// matter, otherwise a restart would resume from the wrong place.
func TestRenderLiveParsePublishedRoundTrip(t *testing.T) {
	pdt := time.Date(2026, 8, 31, 17, 0, 0, 0, time.UTC)
	original := []PublishedSegment{
		{Sequence: 900, URI: "2026/08/31/seg-000000900-1788190000000.ts",
			Duration: 4 * time.Second, ProgramDateTime: pdt},
		{Sequence: 901, URI: "2026/08/31/seg-000000901-1788190004000.ts",
			Duration: 6500 * time.Millisecond, ProgramDateTime: pdt.Add(4 * time.Second),
			Discontinuity: true},
	}
	rendered := RenderLive(original, 7, "write-1")

	pub, err := ParsePublished(rendered)
	if err != nil {
		t.Fatalf("ParsePublished on our own output: %v", err)
	}
	if pub.MaxSequence != 901 {
		t.Errorf("MaxSequence = %d, want 901", pub.MaxSequence)
	}
	if pub.DiscontinuitySequence != 7 {
		t.Errorf("DiscontinuitySequence = %d, want 7", pub.DiscontinuitySequence)
	}
	if len(pub.Window) != len(original) {
		t.Fatalf("window length %d, want %d", len(pub.Window), len(original))
	}
	for i := range original {
		if pub.Window[i].Sequence != original[i].Sequence {
			t.Errorf("[%d] sequence %d, want %d", i, pub.Window[i].Sequence, original[i].Sequence)
		}
		if pub.Window[i].URI != original[i].URI {
			t.Errorf("[%d] uri %q, want %q", i, pub.Window[i].URI, original[i].URI)
		}
		// EXTINF is rendered with millisecond precision.
		if d := pub.Window[i].Duration - original[i].Duration; d > time.Millisecond || d < -time.Millisecond {
			t.Errorf("[%d] duration %v, want %v", i, pub.Window[i].Duration, original[i].Duration)
		}
		if pub.Window[i].Discontinuity != original[i].Discontinuity {
			t.Errorf("[%d] discontinuity %v, want %v", i,
				pub.Window[i].Discontinuity, original[i].Discontinuity)
		}
	}
}

func TestParsePublishedRejectsForeignManifest(t *testing.T) {
	// A playlist written by some other tool. Guessing a sequence from this
	// could overwrite another writer's live segments, so it must error.
	foreign := "#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:6.000,\nchunk_00001.ts\n"
	if _, err := ParsePublished([]byte(foreign)); err == nil {
		t.Fatal("expected an error for a URI that is not ours")
	}
}

func TestParsePublishedRejectsEmptyAndGarbage(t *testing.T) {
	cases := map[string]string{
		"no header":   "#EXTINF:4.000,\n2026/08/31/seg-000000001-1.ts\n",
		"no segments": "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n",
		"empty":       "",
	}
	for name, body := range cases {
		if _, err := ParsePublished([]byte(body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// TestParsePublishedRejectsAForeignPath is the ownership check. Recovery uses
// this manifest to decide it may keep publishing into the same key, so a
// playlist that only happens to contain a similar filename, or one pointing at
// another host, must not qualify.
func TestParsePublishedRejectsAForeignPath(t *testing.T) {
	cases := map[string]string{
		"absolute url":     "https://cdn.example/2026/08/31/seg-000000001-1.ts",
		"leading slash":    "/2026/08/31/seg-000000001-1.ts",
		"bare basename":    "seg-000000001-1.ts",
		"wrong depth":      "2026/08/seg-000000001-1.ts",
		"query string":     "2026/08/31/seg-000000001-1.ts?token=abc",
		"dot segment":      "2026/08/../08/31/seg-000000001-1.ts",
		"non numeric date": "yyyy/mm/dd/seg-000000001-1.ts",
	}
	for name, uri := range cases {
		body := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:4.000,\n" + uri + "\n"
		if _, err := ParsePublished([]byte(body)); err == nil {
			t.Errorf("%s (%q): expected rejection", name, uri)
		}
	}
}

// TestParsePublishedRejectsANonContiguousWindow guards the resume point. A gap
// in the sequence means the playlist is not a window this daemon rendered, and
// resuming from its maximum could reissue a key inside the hole.
func TestParsePublishedRejectsANonContiguousWindow(t *testing.T) {
	body := strings.Join([]string{
		"#EXTM3U", "#EXT-X-VERSION:3", "#EXT-X-MEDIA-SEQUENCE:10",
		"#EXTINF:4.000,", "2026/08/31/seg-000000010-1.ts",
		"#EXTINF:4.000,", "2026/08/31/seg-000000012-2.ts",
		"",
	}, "\n")
	if _, err := ParsePublished([]byte(body)); err == nil {
		t.Fatal("expected a non-contiguous window to be rejected")
	}
}

func TestParsePublishedRejectsMismatchedMediaSequence(t *testing.T) {
	body := strings.Join([]string{
		"#EXTM3U", "#EXT-X-VERSION:3", "#EXT-X-MEDIA-SEQUENCE:99",
		"#EXTINF:4.000,", "2026/08/31/seg-000000010-1.ts",
		"",
	}, "\n")
	if _, err := ParsePublished([]byte(body)); err == nil {
		t.Fatal("EXT-X-MEDIA-SEQUENCE must agree with the first segment")
	}
}

func TestParsePublishedRequiresMediaSequence(t *testing.T) {
	body := "#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:4.000,\n2026/08/31/seg-000000010-1.ts\n"
	if _, err := ParsePublished([]byte(body)); err == nil {
		t.Fatal("every manifest this daemon writes has EXT-X-MEDIA-SEQUENCE")
	}
}
