package roku

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Registry is the saved set of known TVs, so you can say "livingroom"
// instead of an IP address.
//
// Addresses are cached but never trusted: DHCP moves them. Serial numbers
// are the real identity, so a stale address triggers a rediscovery and the
// entry heals itself.
type Registry struct {
	Devices []Device `json:"devices"`

	path string
	mu   sync.Mutex
}

// RegistryPath is where the device list lives, honouring XDG_CONFIG_HOME.
func RegistryPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "roku", "devices.json"), nil
}

// LoadRegistry reads the saved devices. A missing file is not an error:
// it just means nothing has been discovered yet.
func LoadRegistry() (*Registry, error) {
	path, err := RegistryPath()
	if err != nil {
		return nil, err
	}
	r := &Registry{path: path}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}

// Save writes the registry back to disk atomically.
func (r *Registry) Save() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}

// Merge folds freshly discovered devices into the registry, matching on
// serial number so a TV that changed address keeps its alias.
func (r *Registry) Merge(found []Device) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range found {
		idx := -1
		for i, known := range r.Devices {
			if d.Serial != "" && known.Serial == d.Serial {
				idx = i
				break
			}
			if d.Serial == "" && known.Addr == d.Addr {
				idx = i
				break
			}
		}
		if idx < 0 {
			r.Devices = append(r.Devices, d)
			continue
		}
		// Keep user-chosen settings; refresh everything the device reports.
		alias := r.Devices[idx].Name
		profiles := r.Devices[idx].Profiles
		legacy := r.Devices[idx].YouTubeProfile
		r.Devices[idx] = d
		if alias != "" && !strings.EqualFold(alias, d.Name) {
			r.Devices[idx].Name = alias
		}
		if d.Profiles == nil {
			r.Devices[idx].Profiles = profiles
		}
		if d.YouTubeProfile == 0 {
			r.Devices[idx].YouTubeProfile = legacy
		}
	}
	sort.Slice(r.Devices, func(i, j int) bool { return r.Devices[i].Name < r.Devices[j].Name })
}

// SetProfile records which account tile to choose for a channel on a
// device. A tile of zero removes the setting.
func (r *Registry) SetProfile(name, appID string, tile int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, d := range r.Devices {
		if !strings.EqualFold(d.Name, name) && d.Serial != name && d.Addr != NormalizeAddr(name) {
			continue
		}
		if r.Devices[i].Profiles == nil {
			r.Devices[i].Profiles = map[string]int{}
		}
		if tile <= 0 {
			delete(r.Devices[i].Profiles, appID)
		} else {
			r.Devices[i].Profiles[appID] = tile
		}
		// Retire the old single-channel field once it has been superseded.
		if appID == AppYouTube {
			r.Devices[i].YouTubeProfile = 0
		}
		return nil
	}
	return fmt.Errorf("no device named %q", name)
}

// Rename gives a device a friendlier local alias.
func (r *Registry) Rename(old, new string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, d := range r.Devices {
		if strings.EqualFold(d.Name, old) || d.Addr == NormalizeAddr(old) || d.Serial == old {
			r.Devices[i].Name = new
			return nil
		}
	}
	return fmt.Errorf("no device named %q", old)
}

// Match returns the devices a target names: "all" for everything, an alias
// or serial for one, or a bare IP for something not in the registry at all.
func (r *Registry) Match(target string) ([]Device, error) {
	if target == "" || strings.EqualFold(target, "all") {
		if len(r.Devices) == 0 {
			return nil, fmt.Errorf("no saved devices; run `roku discover --save` first")
		}
		return append([]Device(nil), r.Devices...), nil
	}
	for _, d := range r.Devices {
		if strings.EqualFold(d.Name, target) || d.Serial == target {
			return []Device{d}, nil
		}
	}
	// Prefix match, so "living" finds "Living Room TV".
	var hits []Device
	for _, d := range r.Devices {
		if strings.Contains(strings.ToLower(d.Name), strings.ToLower(target)) {
			hits = append(hits, d)
		}
	}
	if len(hits) == 1 {
		return hits, nil
	}
	if len(hits) > 1 {
		var names []string
		for _, d := range hits {
			names = append(names, d.Name)
		}
		return nil, fmt.Errorf("%q is ambiguous: %s", target, strings.Join(names, ", "))
	}
	// Not a known name: treat it as an address and let the request fail if wrong.
	if strings.ContainsAny(target, ".:") {
		return []Device{{Name: target, Addr: NormalizeAddr(target)}}, nil
	}
	return nil, fmt.Errorf("no device matching %q", target)
}

// Resolve returns a live client for d, rediscovering by serial if the
// cached address has gone stale. The healed address is written back to the
// registry, so the next run is fast again.
func (r *Registry) Resolve(ctx context.Context, d Device) (*Client, Device, error) {
	c := NewClientFor(d)
	if err := c.Ping(ctx); err == nil {
		return c, d, nil
	}
	if d.Serial == "" {
		return nil, d, fmt.Errorf("%s is not answering at %s", d.Name, d.Addr)
	}
	found, err := Discover(ctx, 3*time.Second)
	if err != nil {
		return nil, d, err
	}
	for _, f := range found {
		if f.Serial == d.Serial {
			alias := d.Name
			d = f
			if alias != "" {
				d.Name = alias
			}
			r.Merge([]Device{d})
			_ = r.Save()
			return NewClientFor(d), d, nil
		}
	}
	return nil, d, fmt.Errorf("%s (%s) did not answer discovery; is it powered off with Fast TV Start disabled?", d.Name, d.Serial)
}
