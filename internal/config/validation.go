package config

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

func ValidID(id string) bool { return idPattern.MatchString(id) }

func ValidateRosterKey(key string) error {
	const prefix = "_transmux/rosters/"
	if !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, ".json") ||
		!ValidID(strings.TrimSuffix(strings.TrimPrefix(key, prefix), ".json")) {
		return fmt.Errorf("cameras.object_key must be _transmux/rosters/{shard_id}.json")
	}
	return nil
}

// Validate is shared by configuration validation and dynamic roster loading.
// Never include a URL parser's error: it embeds the original URL, including
// any credentials, and roster errors are exposed by the monitoring API.
func (c StaticCamera) Validate() error {
	if !idPattern.MatchString(c.CenterID) {
		return fmt.Errorf("invalid center_id %q: must match %s", c.CenterID, idPattern)
	}
	if !idPattern.MatchString(c.CameraID) {
		return fmt.Errorf("invalid camera_id %q: must match %s", c.CameraID, idPattern)
	}
	key := c.CenterID + "/" + c.CameraID
	if c.ShardID != "" && !ValidID(c.ShardID) {
		return fmt.Errorf("camera %s: invalid shard_id", key)
	}
	if !utf8.ValidString(c.Name) || utf8.RuneCountInString(c.Name) > 100 || hasControl(c.Name) {
		return fmt.Errorf("camera %s: name is too long or contains a control character", key)
	}
	switch c.Audio {
	case "", "none", "copy", "aac":
	default:
		return fmt.Errorf("camera %s: audio must be none, copy or aac", key)
	}
	switch c.Format {
	case "", "mpegts", "fmp4":
	default:
		return fmt.Errorf("camera %s: format must be mpegts or fmp4", key)
	}
	switch c.VideoCodec {
	case "", "auto", "h264":
	case "hevc":
		if c.Format != "fmp4" {
			return fmt.Errorf("camera %s: HEVC requires fmp4 format", key)
		}
	default:
		return fmt.Errorf("camera %s: video_codec must be auto, h264 or hevc", key)
	}
	u, err := url.Parse(c.RTSPURL)
	if err != nil {
		return fmt.Errorf("camera %s: invalid rtsp_url", key)
	}
	switch strings.ToLower(u.Scheme) {
	case "rtsp", "rtsps":
	default:
		return fmt.Errorf("camera %s: rtsp_url scheme must be rtsp or rtsps", key)
	}
	if !validURLHost(u) {
		return fmt.Errorf("camera %s: rtsp_url must have a hostname and a valid port", key)
	}
	return nil
}

// ValidateHTTPURL checks endpoints without exposing query tokens or userinfo
// in configuration errors. Network access is not needed for -validate.
func ValidateHTTPURL(raw, field string) error {
	u, err := url.Parse(raw)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || !validURLHost(u) {
		return fmt.Errorf("%s must be an absolute http or https URL with a valid host and port", field)
	}
	return nil
}

func validURLHost(u *url.URL) bool {
	if u.Hostname() == "" {
		return false
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		return err == nil && n > 0 && n <= 65535
	}
	return !strings.HasSuffix(u.Host, ":")
}

func hasControl(s string) bool {
	return strings.IndexFunc(s, unicode.IsControl) >= 0
}

func validateKeyPrefix(prefix string) error {
	if strings.Contains(prefix, `\`) || hasControl(prefix) {
		return fmt.Errorf("storage.key_prefix contains an unsafe character")
	}
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return nil
	}
	for _, part := range strings.Split(prefix, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("storage.key_prefix contains an unsafe path element")
		}
	}
	return nil
}
