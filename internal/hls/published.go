package hls

import (
	"bufio"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// publishedURIPattern matches the segment URIs this daemon writes, relative to
// the manifest: YYYY/MM/DD/seg-{sequence}-{unixms}.ts. The sequence is
// embedded in the name precisely so it can be recovered from a manifest alone.
//
// The whole relative path is matched, not just the basename. Recovery decides
// whether the daemon owns a manifest and may keep publishing into it, so a
// playlist that merely happens to contain a similar filename, or one that
// points at an absolute URL on another host, must not qualify.
var publishedURIPattern = regexp.MustCompile(`^\d{4}/\d{2}/\d{2}/seg-(\d+)-(\d+)\.ts$`)

// Published is the state recovered from a manifest already in the object
// store.
type Published struct {
	// MaxSequence is the highest media sequence the manifest references.
	MaxSequence uint64
	// DiscontinuitySequence is the value of EXT-X-DISCONTINUITY-SEQUENCE.
	DiscontinuitySequence uint64
	// Window is the live window in playlist order.
	Window []PublishedSegment
	// WriteID is the identity of the write that produced this body, taken
	// from the transmux-write-id comment. It is empty for a manifest written
	// before that comment existed.
	WriteID string
}

// ParsePublished reads a manifest this daemon previously wrote and recovers
// enough state to continue without rewinding.
//
// Why this exists: the media sequence must never go backwards. The local
// checkpoint is the fast path, but it lives on the instance and does not
// survive a container replacement on ECS or EKS. The manifest in the object
// store is the durable record, so it is the authority of last resort.
//
// Only URIs matching the daemon's own naming scheme are accepted. A manifest
// written by some other tool yields an error rather than a wrong sequence,
// because guessing here would overwrite live segments.
func ParsePublished(data []byte) (Published, error) {
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var (
		out            Published
		pending        PublishedSegment
		havePendingInf bool
		sawHeader      bool
		count          int
		mediaSeq       uint64
		sawMediaSeq    bool
	)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "#") {
			if !havePendingInf {
				continue
			}
			seq, err := sequenceFromURI(line)
			if err != nil {
				return Published{}, err
			}
			// Sequences must advance by exactly one. A gap or a repeat means
			// the playlist is not a window this daemon rendered, and resuming
			// from its maximum could reissue a key in the hole.
			if count > 0 {
				prev := out.Window[count-1].Sequence
				if seq != prev+1 {
					return Published{}, fmt.Errorf(
						"segment sequence jumps from %d to %d; the playlist is not one of ours",
						prev, seq)
				}
			} else if sawMediaSeq && seq != mediaSeq {
				return Published{}, fmt.Errorf(
					"EXT-X-MEDIA-SEQUENCE is %d but the first segment is %d", mediaSeq, seq)
			}
			pending.Sequence = seq
			pending.URI = line
			out.Window = append(out.Window, pending)
			if seq > out.MaxSequence {
				out.MaxSequence = seq
			}
			pending = PublishedSegment{}
			havePendingInf = false
			count++
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
				return Published{}, fmt.Errorf("invalid EXTINF %q: %w", line, err)
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
		case strings.HasPrefix(line, WriteIDComment):
			out.WriteID = strings.TrimSpace(strings.TrimPrefix(line, WriteIDComment))
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			v := strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:")
			n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return Published{}, fmt.Errorf("invalid EXT-X-MEDIA-SEQUENCE %q: %w", line, err)
			}
			mediaSeq, sawMediaSeq = n, true
		case strings.HasPrefix(line, "#EXT-X-DISCONTINUITY-SEQUENCE:"):
			v := strings.TrimPrefix(line, "#EXT-X-DISCONTINUITY-SEQUENCE:")
			n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return Published{}, fmt.Errorf("invalid EXT-X-DISCONTINUITY-SEQUENCE %q: %w", line, err)
			}
			out.DiscontinuitySequence = n
		}
	}
	if err := sc.Err(); err != nil {
		return Published{}, err
	}
	if !sawHeader {
		return Published{}, fmt.Errorf("not an m3u8 playlist: missing #EXTM3U")
	}
	if count == 0 {
		// An empty playlist is only ours if it carries our write marker. That
		// is the fence write a new owner makes for a channel that has not
		// published a segment yet; an unmarked empty playlist is someone
		// else's and must not be adopted.
		if out.WriteID == "" {
			return Published{}, fmt.Errorf("playlist references no segments")
		}
	}
	if !sawMediaSeq {
		return Published{}, fmt.Errorf("playlist has no EXT-X-MEDIA-SEQUENCE; " +
			"every manifest this daemon writes has one")
	}
	return out, nil
}

// sequenceFromURI extracts the media sequence encoded in a segment URI.
//
// The URI must be relative and match the daemon's own layout exactly. A
// scheme, a host, a leading slash, a query or a dot segment all mean the entry
// was not written by this code path.
func sequenceFromURI(uri string) (uint64, error) {
	if strings.ContainsAny(uri, "?#\\") || strings.HasPrefix(uri, "/") ||
		strings.Contains(uri, "//") || strings.Contains(uri, "..") {
		return 0, fmt.Errorf("segment URI %q is not a plain relative path", uri)
	}
	m := publishedURIPattern.FindStringSubmatch(uri)
	if m == nil {
		return 0, fmt.Errorf("segment URI %q does not match this daemon's naming scheme", uri)
	}
	seq, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("segment URI %q has an unparseable sequence: %w", uri, err)
	}
	return seq, nil
}
