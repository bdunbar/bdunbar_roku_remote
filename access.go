package roku

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

// ErrRestricted means the device refused an endpoint because its network
// access mode is limited.
//
// Roku firmware 12.5 and later ships with "Control by mobile apps >
// Network access" set to Default, which permits only a small subset of
// ECP: device-info, active-app, the volume and power keys, and channel
// launches. Navigation keys, the channel list, playback state and the
// play-on-roku receiver all return 403 until the mode is set to Permissive.
//
// That setting lives on the TV and cannot be changed over the network,
// which is the point of it.
type ErrRestricted struct{ Path string }

func (e ErrRestricted) Error() string {
	return fmt.Sprintf("%s is blocked by this TV's network access mode.\n"+
		"  Fix it on the TV itself:\n"+
		"    Settings > System > Advanced system settings >\n"+
		"      Control by mobile apps > Network access > Permissive", e.Path)
}

// IsRestricted reports whether err is an ECP permission refusal.
func IsRestricted(err error) bool {
	var e ErrRestricted
	return errors.As(err, &e)
}

// Capabilities records which parts of ECP a device will actually serve.
// Probing is cheap and the answer differs per TV, so the CLI leads with it
// rather than letting users guess at 403s.
type Capabilities struct {
	Permissive bool // full ECP: navigation, queries, local streaming
	Volume     bool // volume and mute keys
	Launch     bool // open channels, including YouTube
	Navigate   bool // Home, arrows, Select, Play
	Apps       bool // list installed channels
	Playback   bool // query what is playing
	Streaming  bool // play-on-roku receiver for local files
	AirPlay    bool // AirPlay receiver
	Developer  bool // developer installer, which gates UI inspection

	// Mode is the device's own name for its access mode, when it reports
	// one.
	Mode string
}

// Summary describes the access mode in one word.
func (c Capabilities) Summary() string {
	if c.Mode != "" {
		switch strings.ToLower(c.Mode) {
		case "permissive":
			return "permissive (full control)"
		case "default":
			return "default (limited: volume, power and channel launch only)"
		case "disabled":
			return "disabled"
		default:
			return c.Mode
		}
	}
	switch {
	case c.Permissive:
		return "permissive (full control)"
	case c.Volume || c.Launch:
		return "default (limited: volume, power and channel launch only)"
	default:
		return "disabled or unreachable"
	}
}

// Probe asks a device what it will allow. Each check is a real request, so
// they run concurrently; none of them change what is on screen.
func (c *Client) Probe(ctx context.Context) (Capabilities, error) {
	info, err := c.DeviceInfo(ctx)
	if err != nil {
		return Capabilities{}, err
	}

	caps := Capabilities{
		Mode:      strings.TrimSpace(info.EcpSettingMode),
		Developer: xmlBool(info.DeveloperEnabled),
		Volume:    true,
		Launch:    true,
		AirPlay:   xmlBool(info.SupportsAirPlay),
	}

	// The device reports its own access mode on current firmware. Trust
	// that when it is there and skip the probing entirely.
	if caps.Mode != "" {
		caps.Permissive = strings.EqualFold(caps.Mode, "permissive")
		caps.Apps = caps.Permissive
		caps.Playback = caps.Permissive
		caps.Navigate = caps.Permissive
		caps.Streaming = caps.Permissive && xmlBool(info.HasPlayOnRoku)
		return caps, nil
	}

	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	check := func(method, path string, set func(bool)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.do(ctx, method, path, nil)
			mu.Lock()
			defer mu.Unlock()
			set(err == nil || !IsRestricted(err))
		}()
	}

	// Only read-only probes: nothing here changes what is on screen.
	//
	// Navigation is deliberately not probed by pressing a key. Every real
	// navigation key moves the UI, and an unknown key such as /keypress/Noop
	// answers 200 whatever the access mode, so it proves nothing. The
	// restricted mode gates navigation, the channel list and playback state
	// together, so the queries stand in for all three.
	check(http.MethodGet, "/query/apps", func(v bool) { caps.Apps = v })
	check(http.MethodGet, "/query/media-player", func(v bool) { caps.Playback = v })
	wg.Wait()

	// Volume, power and launch survive the restricted mode, so they are
	// available whenever the device answers at all.
	caps.Permissive = caps.Apps && caps.Playback
	caps.Navigate = caps.Permissive

	// Streaming needs both permission and hardware support. The device
	// reports the latter directly, and on current firmware it reports
	// false, so no access mode will unlock it.
	caps.Streaming = caps.Permissive && xmlBool(info.HasPlayOnRoku)
	return caps, nil
}
