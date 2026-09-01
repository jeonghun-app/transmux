// Package channel supervises one ffmpeg pipeline per camera.
package channel

import "time"

// State is the observable lifecycle state of a channel.
type State string

const (
	// StateStarting means ffmpeg is running but no segment has completed yet.
	StateStarting State = "starting"
	// StateReceiving means segments are completing normally.
	StateReceiving State = "receiving"
	// StateDisconnected means ffmpeg exited and the retry has not begun.
	StateDisconnected State = "disconnected"
	// StateReconnecting means the worker is waiting out its backoff.
	StateReconnecting State = "reconnecting"
	// StateStopping means shutdown is in progress.
	StateStopping State = "stopping"
	// StateStopped is terminal.
	StateStopped State = "stopped"
	// StateFailed means the channel was disabled because it could not be run
	// safely, for example when its starting media sequence is unknown. It
	// requires operator intervention and is never retried automatically.
	StateFailed State = "failed"
)

// stateCode maps a state to a numeric value for the metrics gauge, since
// Prometheus cannot hold a string.
func stateCode(s State) float64 {
	switch s {
	case StateStarting:
		return 1
	case StateReceiving:
		return 2
	case StateDisconnected:
		return 3
	case StateReconnecting:
		return 4
	case StateStopping:
		return 5
	case StateStopped:
		return 6
	case StateFailed:
		return 7
	default:
		return 0
	}
}

// Snapshot is a point-in-time view of a channel, safe to serialise into the
// health API. It never contains credentials.
type Snapshot struct {
	CenterID string `json:"center_id"`
	CameraID string `json:"camera_id"`
	// SourceURL has any userinfo redacted.
	SourceURL string `json:"source_url"`

	State State `json:"state"`
	// Healthy is false when the channel is not currently receiving or the
	// segment gap has exceeded the alarm threshold.
	Healthy bool `json:"healthy"`

	PID int `json:"ffmpeg_pid"`

	SegmentsPublished uint64 `json:"segments_published"`
	SegmentsLost      uint64 `json:"segments_lost"`
	BytesPublished    int64  `json:"bytes_published"`
	Reconnects        uint64 `json:"reconnects"`
	UploadFailures    uint64 `json:"upload_failures"`

	LastSequence uint64 `json:"last_sequence"`

	LastSegmentAt      *time.Time `json:"last_segment_at,omitempty"`
	SecondsSinceSegment *float64  `json:"seconds_since_segment,omitempty"`
	LastErrorAt        *time.Time `json:"last_error_at,omitempty"`
	LastError          string     `json:"last_error,omitempty"`

	StartedAt time.Time `json:"started_at"`
}
