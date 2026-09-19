package roku

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A Macro is a named sequence of steps replayed against a device.
//
// It exists because ECP is write-only: the TV reports which channel is
// running but nothing about what is on screen or where the focus is, so
// anything beyond "launch a channel" is a sequence of keypresses worked
// out by looking at the television. Recording that sequence once, by name,
// is the difference between a one-off and a tool.
type Macro struct {
	Description string   `json:"description,omitempty"`
	Steps       []string `json:"steps"`
}

// Step grammar, one step per entry:
//
//	launch <name|id> [k=v ...]   open a channel, optionally with parameters
//	press <Key> [count]          send a remote key, optionally repeated
//	<Key> [count]                shorthand for press
//	text <words...>              type text into the focused field
//	wait <duration>              pause
//	wait-playing [duration]      wait until something is playing
//	select-until-playing [dur]   press Select until something plays
//
// Arguments passed to Run substitute for $1 ... $9 and $* anywhere in a
// step, so one macro can serve many videos or searches.

// MacroSet is the saved collection of macros.
type MacroSet struct {
	Macros map[string]Macro `json:"macros"`

	path string
	mu   sync.Mutex
}

// MacroPath is where macros live, honouring XDG_CONFIG_HOME.
func MacroPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "roku", "macros.json"), nil
}

// LoadMacros reads the saved macros; a missing file is an empty set.
func LoadMacros() (*MacroSet, error) {
	path, err := MacroPath()
	if err != nil {
		return nil, err
	}
	ms := &MacroSet{Macros: map[string]Macro{}, path: path}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ms, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, ms); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if ms.Macros == nil {
		ms.Macros = map[string]Macro{}
	}
	return ms, nil
}

// Save writes the macros back to disk atomically.
func (ms *MacroSet) Save() error {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(ms.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(ms, "", "  ")
	if err != nil {
		return err
	}
	tmp := ms.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, ms.path)
}

// Names lists the macro names in order.
func (ms *MacroSet) Names() []string {
	out := make([]string, 0, len(ms.Macros))
	for name := range ms.Macros {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Get returns a macro by name.
func (ms *MacroSet) Get(name string) (Macro, bool) {
	m, ok := ms.Macros[name]
	return m, ok
}

// Set stores a macro, replacing any macro of the same name.
func (ms *MacroSet) Set(name string, m Macro) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("a macro needs a name")
	}
	if len(m.Steps) == 0 {
		return fmt.Errorf("a macro needs at least one step")
	}
	// Reject unparseable steps now rather than halfway through a replay.
	for _, step := range m.Steps {
		if _, err := parseStep(step, nil); err != nil {
			return err
		}
	}
	ms.mu.Lock()
	defer ms.mu.Unlock()
	ms.Macros[name] = m
	return nil
}

// Remove deletes a macro.
func (ms *MacroSet) Remove(name string) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if _, ok := ms.Macros[name]; !ok {
		return fmt.Errorf("no macro named %q", name)
	}
	delete(ms.Macros, name)
	return nil
}

// ParseSteps splits a semicolon-separated description into steps.
func ParseSteps(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ";") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// step is one parsed instruction.
type step struct {
	op    string
	args  []string
	count int
	dur   time.Duration
}

// parseStep validates a step and resolves any $1..$9 arguments.
func parseStep(s string, args []string) (step, error) {
	s = substitute(strings.TrimSpace(s), args)
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return step{}, fmt.Errorf("empty step")
	}
	op := strings.ToLower(fields[0])
	rest := fields[1:]

	switch op {
	case "wait":
		if len(rest) != 1 {
			return step{}, fmt.Errorf("wait needs a duration, e.g. wait 3s")
		}
		d, err := time.ParseDuration(rest[0])
		if err != nil {
			return step{}, fmt.Errorf("wait: %q is not a duration", rest[0])
		}
		return step{op: "wait", dur: d}, nil

	case "wait-playing", "select-until-playing":
		d := 60 * time.Second
		if len(rest) == 1 {
			parsed, err := time.ParseDuration(rest[0])
			if err != nil {
				return step{}, fmt.Errorf("%s: %q is not a duration", op, rest[0])
			}
			d = parsed
		}
		return step{op: op, dur: d}, nil

	case "launch":
		if len(rest) == 0 {
			return step{}, fmt.Errorf("launch needs a channel name or app ID")
		}
		return step{op: "launch", args: rest}, nil

	case "text":
		if len(rest) == 0 {
			return step{}, fmt.Errorf("text needs something to type")
		}
		return step{op: "text", args: rest}, nil

	case "press":
		if len(rest) == 0 {
			return step{}, fmt.Errorf("press needs a key")
		}
		return keyStep(rest[0], rest[1:])
	}

	// Shorthand: a bare key name, optionally with a repeat count.
	return keyStep(fields[0], rest)
}

func keyStep(name string, rest []string) (step, error) {
	key, ok := IsKey(name)
	if !ok {
		return step{}, fmt.Errorf("unknown key or step %q (try: roku key --list)", name)
	}
	count := 1
	if len(rest) == 1 {
		n, err := strconv.Atoi(rest[0])
		if err != nil || n < 1 {
			return step{}, fmt.Errorf("%s: %q is not a repeat count", key, rest[0])
		}
		count = n
	} else if len(rest) > 1 {
		return step{}, fmt.Errorf("%s takes at most a repeat count", key)
	}
	return step{op: "press", args: []string{key}, count: count}, nil
}

// substitute replaces $1..$9 and $* with the supplied arguments.
func substitute(s string, args []string) string {
	if len(args) > 0 {
		s = strings.ReplaceAll(s, "$*", strings.Join(args, " "))
	}
	for i := 9; i >= 1; i-- {
		token := "$" + strconv.Itoa(i)
		val := ""
		if i <= len(args) {
			val = args[i-1]
		}
		s = strings.ReplaceAll(s, token, val)
	}
	return s
}

// Run replays a macro against a device.
//
// Progress is reported through onStep rather than printed, so a GUI can
// show the same thing without the library knowing what a terminal is.
func (m Macro) Run(ctx context.Context, c *Client, args []string, onStep func(string)) error {
	for i, raw := range m.Steps {
		st, err := parseStep(raw, args)
		if err != nil {
			return fmt.Errorf("step %d (%q): %w", i+1, raw, err)
		}
		if onStep != nil {
			onStep(substitute(strings.TrimSpace(raw), args))
		}
		if err := runStep(ctx, c, st); err != nil {
			return fmt.Errorf("step %d (%q): %w", i+1, raw, err)
		}
	}
	return nil
}

func runStep(ctx context.Context, c *Client, st step) error {
	switch st.op {
	case "wait":
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(st.dur):
			return nil
		}

	case "press":
		return c.PressN(ctx, st.args[0], st.count, 0)

	case "text":
		return c.TypeText(ctx, strings.Join(st.args, " "))

	case "launch":
		app, err := c.FindApp(ctx, st.args[0])
		if err != nil {
			return err
		}
		params, err := parseParams(st.args[1:])
		if err != nil {
			return err
		}
		return c.Launch(ctx, app.ID, params)

	case "wait-playing":
		playing, err := c.WaitPlaying(ctx, st.dur)
		if err != nil {
			return err
		}
		if !playing {
			return fmt.Errorf("nothing started playing within %s", st.dur)
		}
		return nil

	case "select-until-playing":
		return c.selectUntilPlaying(ctx, LaunchOptions{Profile: 1, Wait: st.dur})
	}
	return fmt.Errorf("unknown step %q", st.op)
}
