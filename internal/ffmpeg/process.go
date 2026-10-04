package ffmpeg

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Process is a running ffmpeg instance.
//
// Zombie avoidance rests on three rules, all enforced here:
//   - Wait is called exactly once, by the goroutine started in Start.
//   - stderr is drained to completion; an undrained pipe makes ffmpeg block
//     on a full pipe buffer and stop reading RTSP.
//   - Termination targets the process group, so any helper ffmpeg spawns
//     dies with it. The container must also run under an init process
//     (tini) to reap anything that outlives us.
type Process struct {
	cmd    *exec.Cmd
	log    *slog.Logger
	redact func(string) string
	done   chan struct{}
	waitMu sync.Mutex
	err    error
	// stderrDrained closes when the stderr reader has finished.
	stderrDrained chan struct{}
}

// Start launches ffmpeg. The returned Process is already running.
//
// redact is applied to every stderr line before it is logged. ffmpeg echoes
// its input URL in many of its own error messages, so without it a camera
// password would end up in the container log.
func Start(binary string, args []string, log *slog.Logger, redact func(string) string) (*Process, error) {
	cmd := exec.Command(binary, args...)
	// Own process group so signals reach children too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// No stdin pipe at all: -nostdin plus /dev/null removes any chance of
	// ffmpeg blocking on or consuming terminal input.
	cmd.Stdin = nil
	cmd.Stdout = io.Discard

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	p := &Process{
		cmd:           cmd,
		log:           log,
		redact:        redact,
		done:          make(chan struct{}),
		stderrDrained: make(chan struct{}),
	}

	if err := cmd.Start(); err != nil {
		stderr.Close()
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}

	go p.drainStderr(stderr)
	go func() {
		// Wait must not run until stderr is fully read, otherwise it closes
		// the pipe under the reader.
		<-p.stderrDrained
		p.waitMu.Lock()
		p.err = cmd.Wait()
		p.waitMu.Unlock()
		close(p.done)
	}()
	return p, nil
}

// PID returns the OS process id, or -1 if unavailable.
func (p *Process) PID() int {
	if p.cmd.Process == nil {
		return -1
	}
	return p.cmd.Process.Pid
}

// Done closes once the process has exited and been reaped.
func (p *Process) Done() <-chan struct{} { return p.done }

// Err returns the exit error. Only valid after Done is closed.
func (p *Process) Err() error {
	p.waitMu.Lock()
	defer p.waitMu.Unlock()
	return p.err
}

// Stop terminates the process group: SIGTERM, then SIGKILL after grace.
// It returns once the process has been reaped.
func (p *Process) Stop(grace time.Duration) {
	select {
	case <-p.done:
		return
	default:
	}
	p.signalGroup(syscall.SIGTERM)
	select {
	case <-p.done:
		return
	case <-time.After(grace):
		p.log.Warn("ffmpeg did not exit on SIGTERM, sending SIGKILL", "pid", p.PID())
		p.signalGroup(syscall.SIGKILL)
	}
	// A killed process is always reaped, but bound the wait so a stuck
	// uninterruptible process cannot hang shutdown forever.
	select {
	case <-p.done:
	case <-time.After(grace):
		p.log.Error("ffmpeg process did not reap after SIGKILL", "pid", p.PID())
	}
}

func (p *Process) signalGroup(sig syscall.Signal) {
	if p.cmd.Process == nil {
		return
	}
	pid := p.cmd.Process.Pid
	// Negative pid targets the whole process group created by Setpgid.
	if err := syscall.Kill(-pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		// Fall back to the single process if the group call failed.
		_ = p.cmd.Process.Signal(sig)
	}
}

// maxStderrLines bounds how much of a chatty ffmpeg we log per run so a
// broken camera cannot flood the log pipeline.
const maxStderrLines = 200

// benignStderr matches messages that are expected during normal operation and
// would otherwise generate a warning for every single segment.
//
// The delete-old-segment message is the important one: the uploader removes a
// segment from the spool as soon as it is stored, so ffmpeg's own
// delete_segments later finds it already gone. Keeping both deletions is
// deliberate. Ours reclaims tmpfs immediately, which keeps the spool at
// roughly one segment per channel instead of local_list_size, and ffmpeg's is
// the backstop that bounds the spool if the uploader falls behind.
var benignStderr = []string{
	"failed to delete old segment",
}

func isBenign(line string) bool {
	for _, pat := range benignStderr {
		if strings.Contains(line, pat) {
			return true
		}
	}
	return false
}

func (p *Process) drainStderr(r io.ReadCloser) {
	defer close(p.stderrDrained)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 8*1024), 256*1024)
	logged := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if p.redact != nil {
			line = p.redact(line)
		}
		if isBenign(line) {
			p.log.Debug("ffmpeg", "message", line)
			continue
		}
		if logged < maxStderrLines {
			p.log.Warn("ffmpeg", "message", line)
			logged++
		} else if logged == maxStderrLines {
			p.log.Warn("ffmpeg stderr suppressed for the rest of this run",
				"suppressed_after_lines", maxStderrLines)
			logged++
		}
		// Keep reading even when suppressed: the pipe must stay drained.
	}
	// An over-long line ends the scan early. Say so, because from here on
	// ffmpeg can block writing to a pipe nobody is reading and the stall
	// watchdog, not this goroutine, is what will notice.
	if err := sc.Err(); err != nil {
		p.log.Error("ffmpeg stderr reader stopped early", "error", err)
	}
}

// Probe verifies the ffmpeg binary is present and executable. Called once at
// startup so a broken image fails fast instead of failing per channel.
func Probe(ctx context.Context, binary string) (string, error) {
	path, err := exec.LookPath(binary)
	if err != nil {
		return "", fmt.Errorf("ffmpeg binary %q not found: %w", binary, err)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "-hide_banner", "-version")
	cmd.Stdin = nil
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("run %s -version: %w", path, err)
	}
	first := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	return first, nil
}

// EnsureSpool creates a channel's spool directory, removing any files left
// behind by a previous run. Leftovers are never uploaded: their durations
// are unknown and their sequence numbers belong to a dead generation.
func EnsureSpool(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("clear spool %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create spool %s: %w", dir, err)
	}
	return nil
}
