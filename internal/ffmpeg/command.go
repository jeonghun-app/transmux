// Package ffmpeg builds and supervises the per-channel ffmpeg process.
package ffmpeg

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/jeonghun-app/transmux/internal/config"
)

// SegmentPattern is the local segment filename template. ffmpeg restarts its
// own numbering at zero on every launch; the published object key uses our
// monotonic sequence instead, so local reuse is harmless.
const SegmentPattern = "seg-%06d.ts"

// LocalPlaylistName is the playlist ffmpeg maintains on the spool.
const LocalPlaylistName = "local.m3u8"

// ValidSegmentName reports whether a name from the local playlist is one
// ffmpeg could have written for SegmentPattern.
//
// The playlist is a file on disk and its contents are joined onto the spool
// path, so it is treated as untrusted input: a crafted entry must not be able
// to reach outside the channel's own directory.
func ValidSegmentName(name string) bool {
	return segmentNamePattern.MatchString(name)
}

var segmentNamePattern = regexp.MustCompile(`^seg-\d{1,10}\.(ts|m4s)$`)

// Spec describes one transmux invocation.
type Spec struct {
	RTSPURL    string
	SpoolDir   string
	Cfg        config.FFmpegConfig
	Segment    config.SegmentConfig
	Audio      string
	Format     string
	VideoCodec string
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
	// Rendered as a decimal rather than truncated to whole seconds: a 1500ms
	// target used to become "1", silently halving the segment length the
	// operator asked for.
	secs := s.Segment.TargetDuration.Duration.Seconds()
	if secs < 1 {
		secs = 1
	}
	hlsTime := strconv.FormatFloat(secs, 'f', -1, 64)
	args := []string{"-map", "0:v:0", "-c:v", "copy"}
	switch s.Audio {
	case "", "none":
		args = append(args, "-an")
	case "copy":
		args = append(args, "-map", "0:a:0?", "-c:a", "copy")
	case "aac":
		args = append(args, "-map", "0:a:0?", "-c:a", "aac", "-ar", "48000", "-b:a", "96k")
	default:
		return nil, fmt.Errorf("audio must be none, copy or aac")
	}
	pattern := SegmentPattern
	switch s.Format {
	case "", "mpegts":
	case "fmp4":
		pattern = "seg-%06d.m4s"
		args = append(args, "-hls_segment_type", "fmp4", "-hls_fmp4_init_filename", "init.mp4")
	default:
		return nil, fmt.Errorf("format must be mpegts or fmp4")
	}
	switch s.VideoCodec {
	case "", "auto":
	case "h264":
		if s.Format == "fmp4" {
			args = append(args, "-tag:v", "avc1")
		}
	case "hevc":
		if s.Format != "fmp4" {
			return nil, fmt.Errorf("HEVC requires fmp4 format")
		}
		args = append(args, "-tag:v", "hvc1")
	default:
		return nil, fmt.Errorf("video_codec must be auto, h264 or hevc")
	}
	return append(args, []string{
		"-f", "hls",
		"-hls_time", hlsTime,
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
		"-hls_segment_filename", filepath.Join(s.SpoolDir, pattern),
		filepath.Join(s.SpoolDir, LocalPlaylistName),
	}...), nil
}
