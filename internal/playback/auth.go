package playback

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var errUnauthorized = errors.New("unauthorized")

type Claims struct {
	jwt.RegisteredClaims
	Type   string  `json:"token_type"`
	Grants []Grant `json:"grants,omitempty"`
	Center string  `json:"center_id,omitempty"`
	Camera string  `json:"camera_id,omitempty"`
	Mode   string  `json:"mode,omitempty"`
	Start  int64   `json:"start_ms,omitempty"`
	End    int64   `json:"end_ms,omitempty"`
}

func (c Claims) Allows(center, camera, permission string) bool {
	for _, g := range c.Grants {
		if g.CenterID != "*" && g.CenterID != center {
			continue
		}
		if !slices.Contains(g.CameraIDs, "*") && !slices.Contains(g.CameraIDs, camera) {
			continue
		}
		if slices.Contains(g.Permissions, permission) {
			return true
		}
	}
	return false
}

func (c Claims) GlobalAdmin() bool { return c.Allows("*", "*", "manage") }

type loginUser struct {
	salt   []byte
	hash   []byte
	grants []Grant
}

type Auth struct {
	cfg     AuthConfig
	key     []byte
	users   map[string]loginUser
	dummy   loginUser
	proxies []netip.Prefix
}

func NewAuth(cfg AuthConfig) (*Auth, error) {
	key := []byte(os.Getenv(cfg.SecretEnv))
	if len(key) < 32 {
		return nil, fmt.Errorf("%s must contain at least 32 bytes of random signing material", cfg.SecretEnv)
	}
	proxies, err := parseTrustedProxies(cfg.TrustedProxies)
	if err != nil {
		return nil, err
	}
	a := &Auth{cfg: cfg, key: key, users: make(map[string]loginUser), proxies: proxies}
	for _, u := range cfg.Users {
		password := os.Getenv(u.PasswordEnv)
		if len(password) < 12 || len(password) > 1024 {
			return nil, fmt.Errorf("%s must contain a password of 12..1024 bytes", u.PasswordEnv)
		}
		salt := make([]byte, 16)
		if _, err := rand.Read(salt); err != nil {
			return nil, err
		}
		hash, err := passwordHash(password, salt)
		if err != nil {
			return nil, err
		}
		a.users[u.Username] = loginUser{salt, hash, u.Grants}
	}
	a.dummy = loginUser{salt: make([]byte, 16), hash: make([]byte, 32)}
	return a, nil
}

func passwordHash(password string, salt []byte) ([]byte, error) {
	return pbkdf2.Key(sha256.New, password, salt, 600000, 32)
}

func (a *Auth) Login(username, password string) (string, *Claims, error) {
	if len(username) > 64 || len(password) > 1024 {
		return "", nil, errUnauthorized
	}
	u, exists := a.users[username]
	if !exists {
		u = a.dummy
	}
	hash, err := passwordHash(password, u.salt)
	if err != nil || subtle.ConstantTimeCompare(hash, u.hash) != 1 || !exists {
		return "", nil, errUnauthorized
	}
	now := time.Now()
	c := &Claims{Type: "access", Grants: u.grants, RegisteredClaims: jwt.RegisteredClaims{
		Issuer: a.cfg.Issuer, Subject: username, Audience: jwt.ClaimStrings{a.cfg.Audience},
		IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(a.cfg.AccessTTL.Duration)),
	}}
	token, err := a.sign(c)
	return token, c, err
}

func (a *Auth) sign(c *Claims) (string, error) {
	return jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(a.key)
}

func (a *Auth) Parse(raw string, media bool) (*Claims, error) {
	if len(raw) > 16384 || raw == "" {
		return nil, errUnauthorized
	}
	audience, kind := a.cfg.Audience, "access"
	if media {
		audience, kind = "transmux-media", "media"
	}
	var c Claims
	token, err := jwt.ParseWithClaims(raw, &c, func(_ *jwt.Token) (any, error) { return a.key, nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer(a.cfg.Issuer),
		jwt.WithAudience(audience), jwt.WithExpirationRequired(), jwt.WithIssuedAt(),
		jwt.WithStrictDecoding())
	if err != nil || !token.Valid || c.Type != kind || c.Subject == "" ||
		c.ExpiresAt == nil || c.IssuedAt == nil {
		return nil, errUnauthorized
	}
	if !c.ExpiresAt.Time.After(c.IssuedAt.Time) ||
		c.ExpiresAt.Time.Sub(c.IssuedAt.Time) > a.cfg.AccessTTL.Duration {
		return nil, errUnauthorized
	}
	if !media {
		if err := validateGrants(c.Grants); err != nil {
			return nil, errUnauthorized
		}
	}
	return &c, nil
}

func (a *Auth) Media(actor *Claims, center, camera, mode string, start, end time.Time) (string, *Claims, error) {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return "", nil, err
	}
	now := time.Now()
	expiry := now.Add(a.cfg.SessionTTL.Duration)
	if actor.ExpiresAt == nil || !actor.ExpiresAt.Time.After(now) {
		return "", nil, errUnauthorized
	}
	if actor.ExpiresAt.Time.Before(expiry) {
		expiry = actor.ExpiresAt.Time
	}
	c := &Claims{
		Type: "media", Center: center, Camera: camera, Mode: mode, Start: start.UnixMilli(),
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: a.cfg.Issuer, Subject: actor.Subject, Audience: jwt.ClaimStrings{"transmux-media"},
			IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiry), ID: hex.EncodeToString(id),
		},
	}
	if !end.IsZero() {
		c.End = end.UnixMilli()
	}
	token, err := a.sign(c)
	return token, c, err
}

const (
	// One client may try several accounts (an operator desk, an office NAT),
	// while guessing against one account is bounded independently of how many
	// addresses an attacker controls.
	loginClientLimit  = 30
	loginAccountLimit = 10
	loginWindow       = time.Minute
	loginTrackedKeys  = 10000
)

type loginRate struct {
	At    time.Time
	Count int
}

// loginLimiter is a fixed-window attempt counter. When the table is full a
// new key is refused rather than evicting a key that is being limited.
type loginLimiter struct {
	mu      sync.Mutex
	limit   int
	entries map[string]loginRate
}

func newLoginLimiter(limit int) *loginLimiter {
	return &loginLimiter{limit: limit, entries: make(map[string]loginRate)}
}

func (l *loginLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, value := range l.entries {
		if now.Sub(value.At) >= loginWindow {
			delete(l.entries, k)
		}
	}
	entry, exists := l.entries[key]
	if !exists && len(l.entries) >= loginTrackedKeys {
		return false
	}
	if entry.At.IsZero() {
		entry.At = now
	}
	entry.Count++
	l.entries[key] = entry
	return entry.Count <= l.limit
}

func parseTrustedProxies(values []string) ([]netip.Prefix, error) {
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return nil, fmt.Errorf("auth.trusted_proxies entries must be CIDRs such as 10.0.0.0/8")
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}

func (a *Auth) trusted(addr netip.Addr) bool {
	for _, prefix := range a.proxies {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// ClientKey identifies the login client for rate limiting. X-Forwarded-For
// is honoured only when the connection comes from a configured proxy, and is
// walked from the right so a client cannot choose its key by prepending
// addresses. IPv6 clients are grouped by /64, the smallest routed allocation.
func (a *Auth) ClientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return "remote:" + host
	}
	addr = addr.Unmap().WithZone("")
	if a.trusted(addr) {
		var hops []string
		for _, value := range r.Header.Values("X-Forwarded-For") {
			hops = append(hops, strings.Split(value, ",")...)
		}
		for n := len(hops) - 1; n >= 0; n-- {
			hop, err := netip.ParseAddr(strings.TrimSpace(hops[n]))
			if err != nil {
				break
			}
			addr = hop.Unmap().WithZone("")
			if !a.trusted(addr) {
				break
			}
		}
	}
	if addr.Is6() {
		prefix, _ := addr.Prefix(64)
		return prefix.String()
	}
	return addr.String()
}
