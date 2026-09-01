// Package ffmpeg builds and supervises the per-channel ffmpeg process.
package ffmpeg

import (
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jeonghun-app/transmux/internal/config"
)

// SegmentPattern is the local segment filename template. ffmpeg restarts its
// own numbering at zero on every launch; the published object key uses our
// monotonic sequence instead, so local reuse is harmless.
const SegmentPattern = "seg-%06d.ts"

// LocalPlaylistName is the playlist ffmpeg maintains on the spool.
const LocalPlaylistName = "local.m3u8"

// Spec describes one transmux invocation.
type Spec struct {
	RTSPURL  string
	SpoolDir string
	Cfg      config.FFmpegConfig
	Segment  config.SegmentConfig
}

// BuildArgs returns the full ffmpeg argument vector.
//
// The arguments are passed as a slice to exec.Command, never through a
// shell, so a hostile RTSP URL cannot inject a command.
func BuildArgs(s Spec) ([]string, error) {
	out, err := OutputArgs(s)
	if err != nil {
		return nil, err
	}
	args := GlobalArgs()
	args = append(args, InputArgs(s)...)
	return append(args, out...), nil
}

// GlobalArgs are the options that apply to the whole invocation.
func GlobalArgs() []string {
	return []string{
		"-nostdin",
		"-hide_banner",
		"-loglevel", "warning",
	}
}

// InputArgs are the RTSP demuxer options and the input URL.
//
// TCP is the default transport. It needs no RTP/RTCP port range, which makes
// it the only transport that works unchanged under bridge networking and a
// restrictive security group. UDP can lower latency on a controlled network
// but requires host networking or an explicit published port range.
func InputArgs(s Spec) []string {
	args := []string{"-rtsp_transport", s.Cfg.RTSPTransport}
	args = append(args, s.Cfg.InputArgs...)
	return append(args, "-i", s.RTSPURL)
}

// OutputArgs are the transmux and HLS muxer options.
//
// Codec handling: -c:v copy makes this a transmux. ffmpeg cannot create new
// IDR frames without re-encoding, so -hls_time is a lower bound on the
// segment length; ffmpeg cuts at the first random-access frame at or after
// that point. A camera with a 10 s GOP therefore yields ~10 s segments and
// the fix is a camera-side GOP change, not a muxer flag. split_by_time is
// deliberately not used: cutting at a non-keyframe produces segments that
// cannot be decoded independently.
func OutputArgs(s Spec) ([]string, error) {
	if s.SpoolDir == "" {
		return nil, fmt.Errorf("spool dir required")
	}
	if s.Segment.LocalListSize <= 0 {
		return nil, fmt.Errorf("local_list_size must be > 0")
	}
	hlsTime := int(s.Segment.TargetDuration.Duration / time.Second)
	if hlsTime < 1 {
		hlsTime = 1
	}
	return []string{
		// Video only for now. Audio needs a per-codec HLS policy (AAC is
		// fine in MPEG-TS, G.711 is not) and cameras vary, so it is an
		// explicit follow-up rather than a silent passthrough.
		"-map", "0:v:0",
		"-an",
		"-c:v", "copy",

		"-f", "hls",
		"-hls_time", strconv.Itoa(hlsTime),
		"-hls_list_size", strconv.Itoa(s.Segment.LocalListSize),
		// temp_file: ffmpeg writes <name>.tmp then renames, so the presence
		//   of seg-N.ts means the segment is complete. The uploader relies
		//   on this instead of guessing from file size.
		// delete_segments: bounds the spool. Combined with local_list_size >
		//   live_window it gives the uploader a grace period of
		//   local_list_size * target_duration before a segment is reclaimed.
		// program_date_time: gives each segment a wall-clock anchor, which
		//   the published manifest and the missing-segment alarm both use.
		"-hls_flags", "temp_file+delete_segments+program_date_time",
		"-hls_segment_filename", filepath.Join(s.SpoolDir, SegmentPattern),
		filepath.Join(s.SpoolDir, LocalPlaylistName),
	}, nil
}
