package hls

import (
	"bufio"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// publishedURIPattern matches the segment URIs this daemon writes, of the
// form YYYY/MM/DD/seg-{sequence}-{unixms}.ts. The sequence is embedded in the
// name precisely so it can be recovered from a manifest alone.
var publishedURIPattern = regexp.MustCompile(`^seg-(\d+)-(\d+)\.ts$`)

// Published is the state recovered from a manifest already in the object
// store.
type Published struct {
	// MaxSequence is the highest media sequence the manifest references.
	MaxSequence uint64
	// DiscontinuitySequence is the value of EXT-X-DISCONTINUITY-SEQUENCE.
	DiscontinuitySequence uint64
	// Window is the live window in playlist order.
	Window []PublishedSegment
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
		return Published{}, fmt.Errorf("playlist references no segments")
	}
	return out, nil
}

// sequenceFromURI extracts the media sequence encoded in a segment URI.
func sequenceFromURI(uri string) (uint64, error) {
	m := publishedURIPattern.FindStringSubmatch(path.Base(uri))
	if m == nil {
		return 0, fmt.Errorf("segment URI %q does not match this daemon's naming scheme", uri)
	}
	seq, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("segment URI %q has an unparseable sequence: %w", uri, err)
	}
	return seq, nil
}
