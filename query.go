package roku

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// DeviceInfo is the reply from /query/device-info. Roku returns far more
// fields than this; these are the ones worth surfacing.
type DeviceInfo struct {
	XMLName         xml.Name `xml:"device-info"`
	UDN             string   `xml:"udn"`
	SerialNumber    string   `xml:"serial-number"`
	DeviceID        string   `xml:"device-id"`
	ModelName       string   `xml:"model-name"`
	ModelNumber     string   `xml:"model-number"`
	FriendlyName    string   `xml:"friendly-device-name"`
	UserDeviceName  string   `xml:"user-device-name"`
	DefaultName     string   `xml:"default-device-name"`
	SoftwareVersion string   `xml:"software-version"`
	NetworkType     string   `xml:"network-type"`
	NetworkName     string   `xml:"network-name"`
	// PowerMode is "PowerOn", "DisplayOff" or "Headless".
	PowerMode string `xml:"power-mode"`
	IsTV      string `xml:"is-tv"`
	IsStick   string `xml:"is-stick"`
	Headphone string `xml:"headphones-connected"`
	// HasPlayOnRoku is the device telling us whether it can be handed a
	// URL to play. Current firmware reports false: the receiver that used
	// to do this is gone, so no amount of ECP will stream a local file.
	HasPlayOnRoku string `xml:"has-play-on-roku"`
	// EcpSettingMode is the device's own report of its network access
	// mode: "permissive", "default" or "disabled". Reading it beats
	// inferring the mode from which endpoints return 403.
	EcpSettingMode string `xml:"ecp-setting-mode"`
	// DeveloperEnabled reports whether the developer installer is on,
	// which is what gates /query/sgnodes and the other inspection
	// endpoints.
	DeveloperEnabled string `xml:"developer-enabled"`
	SecureDevice     string `xml:"secure-device"`
	VendorName       string `xml:"vendor-name"`
	ModelRegion      string `xml:"model-region"`
	SupportsAirPlay  string `xml:"supports-airplay"`
	SupportsSuspend  string `xml:"supports-suspend"`
	SupportsWakeWLAN string `xml:"supports-wake-on-wlan"`
}

// Label picks the most human of the several name fields Roku offers.
func (i DeviceInfo) Label() string {
	for _, s := range []string{i.UserDeviceName, i.FriendlyName, i.DefaultName, i.ModelName} {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return ""
}

// ApplyInfo copies the interesting parts of a device-info reply onto d.
func (d *Device) ApplyInfo(i *DeviceInfo) {
	if n := i.Label(); n != "" {
		d.Name = n
	}
	if i.SerialNumber != "" {
		d.Serial = i.SerialNumber
	}
	d.Model = strings.TrimSpace(i.ModelName)
	d.IsTV = xmlBool(i.IsTV)
	d.HasPlayOnRoku = xmlBool(i.HasPlayOnRoku)
	d.AirPlay = xmlBool(i.SupportsAirPlay)
}

func xmlBool(s string) bool {
	b, _ := strconv.ParseBool(strings.TrimSpace(s))
	return b
}

// DeviceInfo asks the device to describe itself.
func (c *Client) DeviceInfo(ctx context.Context) (*DeviceInfo, error) {
	body, err := c.do(ctx, http.MethodGet, "/query/device-info", nil)
	if err != nil {
		return nil, err
	}
	var info DeviceInfo
	if err := xml.Unmarshal(body, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// App is an installed channel.
type App struct {
	ID      string `xml:"id,attr"`
	Type    string `xml:"type,attr"`
	Version string `xml:"version,attr"`
	Name    string `xml:",chardata"`
}

type appList struct {
	XMLName xml.Name `xml:"apps"`
	Apps    []App    `xml:"app"`
}

// Apps lists the installed channels and their IDs.
func (c *Client) Apps(ctx context.Context) ([]App, error) {
	body, err := c.do(ctx, http.MethodGet, "/query/apps", nil)
	if err != nil {
		return nil, err
	}
	var l appList
	if err := xml.Unmarshal(body, &l); err != nil {
		return nil, err
	}
	for i := range l.Apps {
		l.Apps[i].Name = strings.TrimSpace(l.Apps[i].Name)
	}
	return l.Apps, nil
}

// FindApp resolves a channel by app ID or by name.
func (c *Client) FindApp(ctx context.Context, q string) (*App, error) {
	apps, err := c.Apps(ctx)
	if err != nil {
		return nil, err
	}
	return MatchApp(apps, q)
}

// MatchApp picks the channel a query names, preferring the most specific
// interpretation.
//
// Ranking matters more than it looks. A Roku carries several channels whose
// names contain each other — "YouTube" is a substring of "YouTube TV", and
// the longer one often comes first in the device's list. Taking the first
// substring hit therefore silently controls the wrong channel, which is
// worse than refusing: the command succeeds and the TV does something else.
//
// So an exact name wins over a prefix, a prefix wins over a substring, and
// anything still ambiguous is reported rather than guessed.
func MatchApp(apps []App, q string) (*App, error) {
	q = strings.TrimSpace(q)
	for i := range apps {
		if apps[i].ID == q {
			return &apps[i], nil
		}
	}

	lower := strings.ToLower(q)
	var exact, prefix, contains []*App
	for i := range apps {
		name := strings.ToLower(apps[i].Name)
		switch {
		case name == lower:
			exact = append(exact, &apps[i])
		case strings.HasPrefix(name, lower):
			prefix = append(prefix, &apps[i])
		case strings.Contains(name, lower):
			contains = append(contains, &apps[i])
		}
	}
	for _, tier := range [][]*App{exact, prefix, contains} {
		switch len(tier) {
		case 0:
			continue
		case 1:
			return tier[0], nil
		default:
			var names []string
			for _, a := range tier {
				names = append(names, fmt.Sprintf("%s (%s)", a.Name, a.ID))
			}
			return nil, fmt.Errorf("%q matches several channels: %s; use the app ID",
				q, strings.Join(names, ", "))
		}
	}
	return nil, errNotFound{q}
}

type errNotFound struct{ q string }

func (e errNotFound) Error() string { return "no channel matching " + strconv.Quote(e.q) }

type activeApp struct {
	XMLName xml.Name `xml:"active-app"`
	App     App      `xml:"app"`
	Screen  string   `xml:"screensaver"`
}

// ActiveApp reports what is on screen now. On the home screen the returned
// app is the pseudo-channel named "Roku".
func (c *Client) ActiveApp(ctx context.Context) (*App, error) {
	body, err := c.do(ctx, http.MethodGet, "/query/active-app", nil)
	if err != nil {
		return nil, err
	}
	var a activeApp
	if err := xml.Unmarshal(body, &a); err != nil {
		return nil, err
	}
	a.App.Name = strings.TrimSpace(a.App.Name)
	return &a.App, nil
}

// MediaPlayer is the reply from /query/media-player: what is playing and
// where we are in it.
type MediaPlayer struct {
	XMLName xml.Name `xml:"player"`
	// State is "play", "pause", "close", "startup" or "error".
	State  string `xml:"state,attr"`
	Error  string `xml:"error,attr"`
	Plugin struct {
		ID   string `xml:"id,attr"`
		Name string `xml:"name,attr"`
	} `xml:"plugin"`
	Format struct {
		Audio     string `xml:"audio,attr"`
		Video     string `xml:"video,attr"`
		Container string `xml:"container,attr"`
	} `xml:"format"`
	// Position and Duration arrive as "12345 ms".
	Position string `xml:"position"`
	Duration string `xml:"duration"`
	IsLive   string `xml:"is_live"`
}

// Playing reports whether the player has media loaded, including media
// that is paused.
func (m *MediaPlayer) Playing() bool {
	switch m.State {
	case "play", "pause", "startup", "buffer":
		return true
	}
	return false
}

// Started reports whether playback is actually under way. Unlike Playing
// it excludes "pause", which matters when deciding whether to send another
// key: a Select aimed at a menu but landing on a playing video pauses it.
func (m *MediaPlayer) Started() bool {
	switch m.State {
	case "play", "startup", "buffer":
		return true
	}
	return false
}

// Paused reports whether media is loaded but halted.
func (m *MediaPlayer) Paused() bool { return m.State == "pause" }

// MediaPlayer queries current playback state.
func (c *Client) MediaPlayer(ctx context.Context) (*MediaPlayer, error) {
	body, err := c.do(ctx, http.MethodGet, "/query/media-player", nil)
	if err != nil {
		return nil, err
	}
	var m MediaPlayer
	if err := xml.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	return &m, nil
}
