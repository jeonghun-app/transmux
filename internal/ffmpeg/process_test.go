package ffmpeg

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

func TestStderrHelperProcess(t *testing.T) {
	if len(os.Args) == 0 || os.Args[len(os.Args)-1] != "transmux-long-stderr" {
		return
	}
	_, _ = io.WriteString(os.Stderr, strings.Repeat("x", 512*1024)+"\nfinished\n")
	os.Exit(0)
}

func TestProcessDrainsAnOversizedStderrLine(t *testing.T) {
	p, err := Start(os.Args[0], []string{"-test.run=^TestStderrHelperProcess$", "--", "transmux-long-stderr"},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Stop(100 * time.Millisecond) })
	select {
	case <-p.Done():
		if err := p.Err(); err != nil {
			t.Fatalf("child failed to finish writing stderr: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("oversized stderr blocked the child process")
	}
}
