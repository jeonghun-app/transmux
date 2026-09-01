// Package hls parses the playlist ffmpeg writes locally and renders the
// playlist that is published to object storage.
//
// Two playlists exist on purpose:
//
//   - The local playlist is ffmpeg's own output. It is the authoritative
//     source of which segments are finished and how long each one actually
//     is. Because we never transcode, the real duration is set by the
//     camera's IDR interval and can differ from the configured target.
//   - The published playlist is rendered here. It uses our own monotonic
//     media sequence and our own object keys, so an ffmpeg restart (which
//     resets ffmpeg's local numbering to zero) can never overwrite an
//     already-published segment or roll the media sequence backwards.
package hls

import (
	"bufio"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// LocalSegment is one finished segment as reported by ffmpeg.
type LocalSegment struct {
	Name            string
	Duration        time.Duration
	ProgramDateTime time.Time // zero when ffmpeg did not emit the tag
	Discontinuity   bool
}

// ParseLocal reads an ffmpeg-generated media playlist.
//
// Tags are accumulated until a URI line is reached, which makes the parser
// insensitive to the relative order of EXTINF and EXT-X-PROGRAM-DATE-TIME.
func ParseLocal(data []byte) ([]LocalSegment, error) {
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var (
		out           []LocalSegment
		pending       LocalSegment
		havePendingInf bool
		sawHeader     bool
	)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "#") {
			if !havePendingInf {
				// A URI with no preceding EXTINF is malformed; skip it
				// rather than publishing a segment of unknown duration.
				continue
			}
			pending.Name = line
			out = append(out, pending)
			pending = LocalSegment{}
			havePendingInf = false
			continue
		}
		switch {
		case line == "#EXTM3U":
			sawHeader = true
		case strings.HasPrefix(line, "#EXTINF:"):
			v := strings.TrimPrefix(line, "#EXTINF:")
			if i := strings.IndexByte(v, ','); i >= 0 {
				v = v[:i]
			}
			secs, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil {
				return nil, fmt.Errorf("invalid EXTINF %q: %w", line, err)
			}
			pending.Duration = time.Duration(secs * float64(time.Second))
			havePendingInf = true
		case strings.HasPrefix(line, "#EXT-X-PROGRAM-DATE-TIME:"):
			raw := strings.TrimPrefix(line, "#EXT-X-PROGRAM-DATE-TIME:")
			if t, err := parsePDT(strings.TrimSpace(raw)); err == nil {
				pending.ProgramDateTime = t
			}
		case line == "#EXT-X-DISCONTINUITY":
			pending.Discontinuity = true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !sawHeader {
		return nil, fmt.Errorf("not an m3u8 playlist: missing #EXTM3U")
	}
	return out, nil
}

// parsePDT accepts both the RFC3339 form and ffmpeg's "+0000" offset form.
func parsePDT(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999-0700", "2006-01-02T15:04:05-0700"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised EXT-X-PROGRAM-DATE-TIME %q", s)
}

// PublishedSegment is one entry of the manifest we upload.
type PublishedSegment struct {
	// Sequence is our monotonic media sequence number for the channel.
	Sequence uint64
	// URI is relative to the manifest's own location.
	URI string
	Duration time.Duration
	ProgramDateTime time.Time
	Discontinuity bool
	// Bytes is the uploaded object size, used for logging and metrics.
	Bytes int64
}

// RenderLive builds a live media playlist.
//
// discontinuitySequence must be the running count of discontinuities that
// have already scrolled out of the window; players need it to keep their
// timeline consistent across a reconnect.
func RenderLive(segments []PublishedSegment, discontinuitySequence uint64) []byte {
	var b strings.Builder
	maxDur := time.Duration(0)
	for _, s := range segments {
		if s.Duration > maxDur {
			maxDur = s.Duration
		}
	}
	// EXT-X-TARGETDURATION must be the rounded-up maximum of the actual
	// EXTINF values, not the configured target. A camera with a long GOP
	// produces longer segments and an understated target duration makes
	// strict players stall.
	target := int(math.Ceil(maxDur.Seconds()))
	if target < 1 {
		target = 1
	}
	mediaSeq := uint64(0)
	if len(segments) > 0 {
		mediaSeq = segments[0].Sequence
	}

	b.WriteString("#EXTM3U\n")
	// Version 3 is the minimum that allows float EXTINF. Nothing here needs
	// a higher version for MPEG-TS segments.
	b.WriteString("#EXT-X-VERSION:3\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", target)
	fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n", mediaSeq)
	fmt.Fprintf(&b, "#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", discontinuitySequence)
	for _, s := range segments {
		if s.Discontinuity {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n", s.Duration.Seconds())
		if !s.ProgramDateTime.IsZero() {
			fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n",
				s.ProgramDateTime.UTC().Format("2006-01-02T15:04:05.000Z"))
		}
		b.WriteString(s.URI)
		b.WriteString("\n")
	}
	return []byte(b.String())
}

// ContentTypeManifest is the media type registered for HLS playlists.
const ContentTypeManifest = "application/vnd.apple.mpegurl"

// ContentTypeSegment is the media type for an MPEG-TS segment.
const ContentTypeSegment = "video/mp2t"
