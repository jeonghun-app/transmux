package channel

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jeonghun-app/transmux/internal/hls"
)

// checkpoint persists the state that must not roll backwards across an
// ffmpeg restart or a daemon restart.
//
// The published media sequence is monotonic per camera. If it reset to zero
// on restart, newly published segments would overwrite existing object keys
// and players would see the playlist jump backwards. ffmpeg's own local
// numbering does reset; that is why the object key never uses it.
type checkpoint struct {
	LastSequence          uint64            `json:"last_sequence"`
	DiscontinuitySequence uint64            `json:"discontinuity_sequence"`
	Window                []checkpointEntry `json:"window"`
	UpdatedAt             time.Time         `json:"updated_at"`
}

type checkpointEntry struct {
	Sequence        uint64    `json:"sequence"`
	URI             string    `json:"uri"`
	DurationSeconds float64   `json:"duration_seconds"`
	ProgramDateTime time.Time `json:"program_date_time,omitempty"`
	Discontinuity   bool      `json:"discontinuity"`
	Bytes           int64     `json:"bytes"`
}

func (c *checkpoint) toSegments() []hls.PublishedSegment {
	out := make([]hls.PublishedSegment, 0, len(c.Window))
	for _, e := range c.Window {
		out = append(out, hls.PublishedSegment{
			Sequence:        e.Sequence,
			URI:             e.URI,
			Duration:        time.Duration(e.DurationSeconds * float64(time.Second)),
			ProgramDateTime: e.ProgramDateTime,
			Discontinuity:   e.Discontinuity,
			Bytes:           e.Bytes,
		})
	}
	return out
}

func windowToCheckpoint(segs []hls.PublishedSegment) []checkpointEntry {
	out := make([]checkpointEntry, 0, len(segs))
	for _, s := range segs {
		out = append(out, checkpointEntry{
			Sequence:        s.Sequence,
			URI:             s.URI,
			DurationSeconds: s.Duration.Seconds(),
			ProgramDateTime: s.ProgramDateTime,
			Discontinuity:   s.Discontinuity,
			Bytes:           s.Bytes,
		})
	}
	return out
}

// checkpointPath places each camera under its own center directory.
//
// A flat "{center}_{camera}.json" name is not safe: identifiers may contain
// underscores, so center "a_b" camera "c" and center "a" camera "b_c" would
// collide on one file and overwrite each other's sequence.
func checkpointPath(stateDir, centerID, cameraID string) string {
	return filepath.Join(stateDir, centerID, cameraID+".json")
}

// loadCheckpoint returns a zero checkpoint when none exists. A corrupt file
// is reported so the operator sees it, but startup still proceeds from zero
// rather than refusing to serve the channel.
func loadCheckpoint(path string) (checkpoint, error) {
	var cp checkpoint
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cp, nil
		}
		return cp, err
	}
	if err := json.Unmarshal(raw, &cp); err != nil {
		return checkpoint{}, fmt.Errorf("corrupt checkpoint %s: %w", path, err)
	}
	return cp, nil
}

// saveCheckpoint writes atomically so a crash mid-write cannot leave a
// truncated file that would reset the sequence on the next start.
func saveCheckpoint(path string, cp checkpoint) error {
	cp.UpdatedAt = time.Now().UTC()
	raw, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ckpt-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
