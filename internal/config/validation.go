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

// secretQueryKeys are lower-case fragments of query parameter names that
// cameras and relays use for credentials. Matching is by substring so that
// access_token, ApiKey and X-Amz-Signature are caught along with the plain
// names; a false match only refuses a URL, it never lets one through.
var secretQueryKeys = []string{"pass", "pwd", "token", "key", "secret", "auth", "signature", "credential"}

// CheckCredentialFreeURL reports an error when rtsp_url carries credentials:
// any userinfo (a user name alone included, so there is no partial form to
// argue about) or a query parameter whose name looks like a secret. It is
// enforced only when cameras.require_credential_free_urls is on.
//
// The error names the camera but never quotes the URL or the parameter
// name, because a bare query key can itself be the secret.
func (c StaticCamera) CheckCredentialFreeURL() error {
	key := c.CenterID + "/" + c.CameraID
	u, err := url.Parse(c.RTSPURL)
	if err != nil {
		return fmt.Errorf("camera %s: invalid rtsp_url", key)
	}
	if u.User != nil {
		return fmt.Errorf("camera %s: rtsp_url must not carry userinfo while "+
			"cameras.require_credential_free_urls is on", key)
	}
	if querySecret(u.RawQuery) {
		return fmt.Errorf("camera %s: rtsp_url must not carry credentials in its query while "+
			"cameras.require_credential_free_urls is on", key)
	}
	return nil
}

// querySecret reports whether a raw query has a parameter name that looks
// like a credential. It splits on both & and ; because cameras use either,
// and treats a name it cannot unescape as a secret: it cannot tell.
func querySecret(raw string) bool {
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == '&' || r == ';' }) {
		name, _, _ := strings.Cut(part, "=")
		name, err := url.QueryUnescape(name)
		if err != nil {
			return true
		}
		name = strings.ToLower(name)
		for _, secret := range secretQueryKeys {
			if strings.Contains(name, secret) {
				return true
			}
		}
	}
	return false
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
