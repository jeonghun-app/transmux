package ffmpeg

import (
	"strings"
	"testing"
	"time"

	"github.com/jeonghun-app/transmux/internal/config"
)

func spec(target time.Duration) Spec {
	return Spec{
		RTSPURL:  "rtsp://cam/live",
		SpoolDir: "/spool/c1/cam1",
		Cfg:      config.Default().FFmpeg,
		Segment: config.SegmentConfig{
			TargetDuration: config.Duration{Duration: target},
			LiveWindow:     4,
			LocalListSize:  10,
		},
	}
}

func argValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// TestOutputArgsKeepAFractionalTarget covers a target that is not a whole
// number of seconds. Truncating to an integer silently halved a 1500ms target.
func TestOutputArgsKeepAFractionalTarget(t *testing.T) {
	args, err := OutputArgs(spec(1500 * time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if got := argValue(args, "-hls_time"); got != "1.5" {
		t.Errorf("-hls_time = %q, want 1.5", got)
	}
}

func TestOutputArgsRenderWholeSecondsPlainly(t *testing.T) {
	args, err := OutputArgs(spec(4 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if got := argValue(args, "-hls_time"); got != "4" {
		t.Errorf("-hls_time = %q, want 4", got)
	}
}

// The muxer flags are load-bearing: temp_file is what makes a visible
// seg-N.ts mean "complete", and program_date_time is the wall-clock anchor the
// object layout and the gap alarm both depend on.
func TestOutputArgsKeepTheLoadBearingFlags(t *testing.T) {
	args, err := OutputArgs(spec(4 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	flags := argValue(args, "-hls_flags")
	for _, want := range []string{"temp_file", "delete_segments", "program_date_time"} {
		if !strings.Contains(flags, want) {
			t.Errorf("-hls_flags = %q, missing %s", flags, want)
		}
	}
	if got := argValue(args, "-c:v"); got != "copy" {
		t.Errorf("-c:v = %q: this must stay a transmux", got)
	}
	if !contains(args, "-an") {
		t.Error("audio must stay disabled until a per-codec policy exists")
	}
}

func contains(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// The URL goes into the argv slice, never through a shell, so metacharacters
// cannot become a command.
func TestInputArgsPassTheURLAsASingleArgument(t *testing.T) {
	s := spec(4 * time.Second)
	s.RTSPURL = "rtsp://cam/live; rm -rf /"
	args := InputArgs(s)
	if args[len(args)-1] != s.RTSPURL {
		t.Errorf("last arg = %q, want the URL verbatim", args[len(args)-1])
	}
	if args[len(args)-2] != "-i" {
		t.Errorf("the URL must directly follow -i, got %q", args[len(args)-2])
	}
}

func TestValidSegmentName(t *testing.T) {
	valid := []string{"seg-0.ts", "seg-000000.ts", "seg-1234567890.ts"}
	for _, n := range valid {
		if !ValidSegmentName(n) {
			t.Errorf("ValidSegmentName(%q) = false, want true", n)
		}
	}
	invalid := []string{
		"../seg-1.ts", "/etc/passwd", "seg-1.ts.tmp", "local.m3u8",
		"seg-.ts", "seg-1.TS", "", "sub/seg-1.ts", "seg-1.ts\n",
	}
	for _, n := range invalid {
		if ValidSegmentName(n) {
			t.Errorf("ValidSegmentName(%q) = true, want false", n)
		}
	}
}
