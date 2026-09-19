// Package roku speaks ECP (External Control Protocol), the plain-HTTP remote
// control interface every Roku device exposes on TCP port 8060.
//
// There is no authentication and no handshake: you find the device, then you
// POST to it. The whole package is stdlib-only and deliberately free of any
// printing or process control, so the same API serves a CLI, a GUI, or a
// web handler.
package roku

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultPort is the fixed TCP port ECP listens on. It is not configurable
// on the device side.
const DefaultPort = 8060

// Device is one Roku on the network. The zero value is not useful; devices
// come from Discover or from the saved Registry.
type Device struct {
	// Name is the human label: the TV's own name if we learned it from
	// /query/device-info, otherwise whatever the user aliased it to.
	Name string `json:"name"`
	// Addr is host:port, always with an explicit port.
	Addr string `json:"addr"`
	// Serial is stable across DHCP lease changes, so it — not Addr — is the
	// real identity of a TV.
	Serial string `json:"serial"`
	Model  string `json:"model,omitempty"`
	// IsTV distinguishes a Roku TV (has volume and power) from a stick or
	// box (does not).
	IsTV bool `json:"is_tv"`
	// HasPlayOnRoku reports whether the device accepts a URL to play.
	// Current firmware says false on every model we have seen.
	HasPlayOnRoku bool `json:"has_play_on_roku"`
	// AirPlay reports AirPlay receiver support, which is the remaining
	// built-in route for local media on modern firmware.
	AirPlay bool `json:"airplay"`
	// Profiles maps a channel's app ID to the account tile to choose when
	// that channel shows a picker, counted from whichever tile has focus.
	Profiles map[string]int `json:"profiles,omitempty"`
	// YouTubeProfile is the previous single-channel form of Profiles, kept
	// so existing device files keep working. Read through Profile.
	YouTubeProfile int `json:"youtube_profile,omitempty"`
}

// Profile returns the account tile configured for a channel, or zero if
// the picker should be left alone.
func (d Device) Profile(appID string) int {
	if n, ok := d.Profiles[appID]; ok {
		return n
	}
	// Fall back to the superseded YouTube-only field.
	if appID == AppYouTube {
		return d.YouTubeProfile
	}
	return 0
}

func (d Device) String() string {
	s := d.Name
	if s == "" {
		s = d.Addr
	}
	if d.Model != "" {
		s += " (" + d.Model + ")"
	}
	return s
}

// Client talks to a single device.
type Client struct {
	Addr string
	HTTP *http.Client
}

// NewClient returns a client for addr, which may be a bare IP or host:port.
func NewClient(addr string) *Client {
	return &Client{
		Addr: NormalizeAddr(addr),
		HTTP: &http.Client{Timeout: 30 * time.Second},
	}
}

// NewClientFor returns a client for an already-discovered device.
func NewClientFor(d Device) *Client { return NewClient(d.Addr) }

// NormalizeAddr appends the default ECP port when the caller gave a bare host.
func NormalizeAddr(addr string) string {
	addr = strings.TrimSpace(addr)
	addr = strings.TrimPrefix(addr, "http://")
	addr = strings.TrimSuffix(addr, "/")
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return net.JoinHostPort(addr, strconv.Itoa(DefaultPort))
	}
	return addr
}

// BaseURL is the root of the device's ECP endpoints.
func (c *Client) BaseURL() string { return "http://" + c.Addr }

// do performs one ECP request and returns the body. ECP replies are small
// XML documents (or empty, for keypresses), so reading them whole is fine.
func (c *Client) do(ctx context.Context, method, path string, q url.Values) ([]byte, error) {
	// Ordinary ECP calls answer immediately; only launches are slow, and
	// they set their own deadline before calling in here.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
	}
	u := c.BaseURL() + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusForbidden {
		return body, ErrRestricted{Path: path}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return body, fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	return body, nil
}

// Ping reports whether the device answers ECP at all. Useful for deciding
// that a cached IP has gone stale.
func (c *Client) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err := c.do(ctx, http.MethodGet, "/query/device-info", nil)
	return err
}

// Keys that ECP accepts on /keypress. Roku ignores unknown keys silently,
// which makes typos invisible, so the CLI validates against this set.
var Keys = []string{
	"Home", "Rev", "Fwd", "Play", "Select",
	"Left", "Right", "Down", "Up",
	"Back", "InstantReplay", "Info",
	"Backspace", "Search", "Enter",
	"VolumeUp", "VolumeDown", "VolumeMute",
	"Power", "PowerOff", "PowerOn",
	"ChannelUp", "ChannelDown", "InputHDMI1", "InputHDMI2",
	"InputHDMI3", "InputHDMI4", "InputAV1", "InputTuner",
	"FindRemote",
}

// IsKey reports whether name is a known ECP key, case-insensitively, and
// returns the canonical spelling.
func IsKey(name string) (string, bool) {
	for _, k := range Keys {
		if strings.EqualFold(k, name) {
			return k, true
		}
	}
	return "", false
}

// Press sends a single key. The device acts on it immediately; there is no
// acknowledgement beyond the HTTP 200.
func (c *Client) Press(ctx context.Context, key string) error {
	_, err := c.do(ctx, http.MethodPost, "/keypress/"+url.PathEscape(key), nil)
	return err
}

// PressN sends a key n times with a gap between presses. Roku drops presses
// that arrive back to back with no delay, which is why the gap is not
// optional; 120ms is comfortably reliable.
func (c *Client) PressN(ctx context.Context, key string, n int, gap time.Duration) error {
	if gap <= 0 {
		gap = 120 * time.Millisecond
	}
	for i := 0; i < n; i++ {
		if err := c.Press(ctx, key); err != nil {
			return err
		}
		if i < n-1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(gap):
			}
		}
	}
	return nil
}

// Volume nudges the volume by delta steps: positive up, negative down.
// Only Roku TVs respond; sticks and boxes have no volume of their own.
func (c *Client) Volume(ctx context.Context, delta int) error {
	key := "VolumeUp"
	if delta < 0 {
		key, delta = "VolumeDown", -delta
	}
	return c.PressN(ctx, key, delta, 0)
}

// TypeText sends a string one character at a time using the Lit_ key form,
// which is how you fill in a search box remotely.
func (c *Client) TypeText(ctx context.Context, s string) error {
	for _, r := range s {
		key := "Lit_" + url.PathEscape(string(r))
		if _, err := c.do(ctx, http.MethodPost, "/keypress/"+key, nil); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(60 * time.Millisecond):
		}
	}
	return nil
}

// Launch starts a channel by its app ID, passing any extra parameters
// through as the query string. Channels interpret those themselves.
//
// A launch does not return until the channel has loaded, which on a cold
// start can take longer than an ordinary ECP call. Rather than fail a
// launch that actually worked, a timeout is confirmed against the active
// app before it is reported as an error.
func (c *Client) Launch(ctx context.Context, appID string, params url.Values) error {
	launchCtx, cancel := context.WithTimeout(ctx, launchTimeout)
	defer cancel()

	_, err := c.do(launchCtx, http.MethodPost, "/launch/"+url.PathEscape(appID), params)
	if err == nil {
		return nil
	}
	if IsRestricted(err) {
		return err
	}

	// The channel may well be loading right now, with the HTTP reply still
	// owed to us. Ask the device what is on screen and believe that instead.
	checkCtx, cancelCheck := context.WithTimeout(ctx, 5*time.Second)
	defer cancelCheck()
	if app, qerr := c.ActiveApp(checkCtx); qerr == nil && app.ID == appID {
		return nil
	}
	return err
}

// launchTimeout is generous because a cold channel start is slow; ordinary
// ECP calls keep the much shorter client timeout.
const launchTimeout = 25 * time.Second

// Input posts to /input, the side channel channels use for deep links.
func (c *Client) Input(ctx context.Context, appID string, params url.Values) error {
	path := "/input"
	if appID != "" {
		path += "/" + url.PathEscape(appID)
	}
	_, err := c.do(ctx, http.MethodPost, path, params)
	return err
}
