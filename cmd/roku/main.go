// Command roku is a remote control for Roku TVs on the local network.
//
// Everything here is presentation: argument parsing, printing, exit codes.
// All the protocol work lives in the roku package, which knows nothing
// about terminals — that is what keeps a GUI a small job rather than a
// rewrite.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/bdunbar/roku"
	"github.com/bdunbar/roku/dlna"
)

const usage = `roku — control Roku TVs on your network

Usage:
  roku [--tv NAME] <command> [args]

Commands:
  discover [--save]      find Rokus via SSDP; --save records them
  scan [--save] [CIDR]   sweep the subnet for port 8060 when SSDP comes up short
  list                   show saved devices
  name OLD NEW           give a device a shorter alias
  check                  report what this TV's access mode allows
  info                   device details
  status                 what's on screen and what's playing
  apps                   installed channels and their IDs
  vol +N | -N | mute     change volume (Roku TVs only)
  key NAME [count]       send a remote key (roku key --list to see them)
  text "hello"           type text into the focused field
  launch NAME|ID         open a channel by name or app ID, handling its
                         account picker if one is configured
  youtube ID|URL         play a YouTube video
  profile [APP [N]]      show, or set for a channel, which account tile to
                         pick (1 = the focused one; 0 clears it)
  play FILE              stream a local video/photo/audio file to the TV
  power on|off           power the TV on or off

Flags:
  --tv NAME    target device: an alias, a serial, an IP, or "all"
               (defaults to $ROKU_TV, else all saved devices)
  --timeout D  per-command timeout (default 30s)

Macros:
  ECP is write-only: the TV says which channel is running but nothing about
  what is on screen. Anything past "launch a channel" is a key sequence you
  work out by watching the television, so record it once and replay it.

    roku macro set yt "launch youtube contentID=$1; select-until-playing 60s"
    roku --tv living run yt dQw4w9WgXcQ

  Steps, separated by semicolons:
    launch <name|id> [k=v ...]   open a channel, optionally with parameters
    <Key> [count]                send a remote key (or: press <Key> [count])
    text <words...>              type into the focused field
    wait <duration>              pause
    wait-playing [duration]      wait until something plays
    select-until-playing [dur]   press Select until something plays
  $1 to $9 and $* take the arguments given to run.

Examples:
  roku discover --save
  roku name "Living Room TV" living
  roku --tv living vol +5
  roku --tv living youtube https://youtu.be/dQw4w9WgXcQ
  roku serve ~/Videos
  roku --tv all power off
`

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "roku: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	var (
		tv      = flag.String("tv", os.Getenv("ROKU_TV"), "target device alias, serial, IP, or \"all\"")
		timeout = flag.Duration("timeout", 30*time.Second, "per-command timeout")
	)
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		return nil
	}

	// Ctrl-C cancels in-flight work rather than killing us mid-request.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Discovery commands set their own pace: a full subnet sweep takes far
	// longer than a single device command, so the --timeout flag applies
	// only to the per-device path below.
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "discover":
		return cmdDiscover(ctx, rest)
	case "scan":
		scanCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		return cmdScan(scanCtx, rest)
	case "serve":
		// Runs until interrupted; no deadline applies.
		return cmdServe(ctx, rest)
	case "macro":
		return cmdMacro(rest)
	case "list":
		return cmdList()
	case "name":
		return cmdName(rest)
	case "help", "-h", "--help":
		flag.Usage()
		return nil
	}

	// Everything below acts on one or more devices.
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	reg, err := roku.LoadRegistry()
	if err != nil {
		return err
	}
	targets, err := reg.Match(*tv)
	if err != nil {
		return err
	}
	return eachDevice(ctx, reg, targets, func(ctx context.Context, c *roku.Client, d roku.Device) error {
		return dispatch(ctx, reg, c, d, cmd, rest)
	})
}

// dispatch runs one command against one already-resolved device.
func dispatch(ctx context.Context, reg *roku.Registry, c *roku.Client, d roku.Device, cmd string, args []string) error {
	switch cmd {
	case "check":
		caps, err := c.Probe(ctx)
		if err != nil {
			return err
		}
		return printCaps(d, caps)

	case "info":
		info, err := c.DeviceInfo(ctx)
		if err != nil {
			return err
		}
		return printInfo(d, info)

	case "status":
		return printStatus(ctx, c, d)

	case "apps":
		apps, err := c.Apps(ctx)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintf(w, "%s\n", d.Name)
		for _, a := range apps {
			fmt.Fprintf(w, "  %s\t%s\n", a.ID, a.Name)
		}
		return w.Flush()

	case "vol":
		if len(args) == 0 {
			return fmt.Errorf("vol needs +N, -N, or mute")
		}
		if strings.EqualFold(args[0], "mute") {
			return c.Press(ctx, "VolumeMute")
		}
		delta, err := strconv.Atoi(strings.TrimPrefix(args[0], "+"))
		if err != nil {
			return fmt.Errorf("vol: %q is not a step count", args[0])
		}
		if !d.IsTV {
			fmt.Fprintf(os.Stderr, "note: %s is not a Roku TV; it may have no volume of its own\n", d.Name)
		}
		return c.Volume(ctx, delta)

	case "key":
		if len(args) == 0 || args[0] == "--list" {
			fmt.Println(strings.Join(roku.Keys, "\n"))
			return nil
		}
		key, ok := roku.IsKey(args[0])
		if !ok {
			return fmt.Errorf("unknown key %q (try: roku key --list)", args[0])
		}
		n := 1
		if len(args) > 1 {
			var err error
			if n, err = strconv.Atoi(args[1]); err != nil {
				return fmt.Errorf("key: %q is not a count", args[1])
			}
		}
		return c.PressN(ctx, key, n, 0)

	case "text":
		if len(args) == 0 {
			return fmt.Errorf("text needs something to type")
		}
		return c.TypeText(ctx, strings.Join(args, " "))

	case "launch":
		if len(args) == 0 {
			return fmt.Errorf("launch needs a channel name or app ID")
		}
		fs := flag.NewFlagSet("launch", flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		settle := fs.Duration("settle", 12*time.Second,
			"how long to let the channel load before choosing an account")
		profileFlag := fs.Int("profile", -1,
			"account tile to pick (overrides the saved one; 0 leaves the picker alone)")
		rest, err := parseInterspersed(fs, args)
		if err != nil {
			return err
		}
		if len(rest) == 0 {
			return fmt.Errorf("launch needs a channel name or app ID")
		}
		app, err := c.FindApp(ctx, strings.Join(rest, " "))
		if err != nil {
			return err
		}
		profile := d.Profile(app.ID)
		if *profileFlag >= 0 {
			profile = *profileFlag
		}
		fmt.Printf("%s: launching %s (%s)\n", d.Name, app.Name, app.ID)
		if profile > 0 {
			fmt.Printf("  will choose account tile %d after %s\n", profile, *settle)
		}
		return c.LaunchWith(ctx, app.ID, roku.LaunchOptions{
			Profile: profile,
			Settle:  *settle,
		})

	case "youtube":
		fs := flag.NewFlagSet("youtube", flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		// Default to whatever this TV has saved, so the flag is only needed
		// to override it.
		profile := fs.Int("profile", d.Profile(roku.AppYouTube),
			"account tile to pick if YouTube asks (1 = the focused one, 0 = leave it)")
		rest, err := parseInterspersed(fs, args)
		if err != nil {
			return err
		}
		if len(rest) == 0 {
			return fmt.Errorf("youtube needs a video ID or URL")
		}
		return c.PlayYouTubeWith(ctx, rest[0], roku.YouTubeOptions{Profile: *profile})

	case "run":
		if len(args) == 0 {
			return fmt.Errorf("run needs a macro name")
		}
		ms, err := roku.LoadMacros()
		if err != nil {
			return err
		}
		m, ok := ms.Get(args[0])
		if !ok {
			return fmt.Errorf("no macro named %q (try: roku macro list)", args[0])
		}
		return m.Run(ctx, c, args[1:], func(s string) {
			fmt.Printf("  %s: %s\n", d.Name, s)
		})

	case "profile":
		if len(args) == 0 {
			return printProfiles(ctx, c, d)
		}
		app, err := c.FindApp(ctx, args[0])
		if err != nil {
			return err
		}
		if len(args) == 1 {
			if n := d.Profile(app.ID); n > 0 {
				fmt.Printf("%s: %s account tile %d\n", d.Name, app.Name, n)
			} else {
				fmt.Printf("%s: %s picker is left alone\n", d.Name, app.Name)
			}
			return nil
		}
		n, err := strconv.Atoi(args[1])
		if err != nil || n < 0 {
			return fmt.Errorf("profile: want a tile position (1, 2, ...) or 0 to clear, got %q", args[1])
		}
		if err := reg.SetProfile(d.Name, app.ID, n); err != nil {
			return err
		}
		if err := reg.Save(); err != nil {
			return err
		}
		if n == 0 {
			fmt.Printf("%s: %s picker will be left alone\n", d.Name, app.Name)
		} else {
			fmt.Printf("%s: %s account tile set to %d\n", d.Name, app.Name, n)
		}
		return nil

	case "power":
		if len(args) == 0 {
			return fmt.Errorf("power needs on or off")
		}
		switch strings.ToLower(args[0]) {
		case "on":
			return c.Press(ctx, "PowerOn")
		case "off":
			return c.Press(ctx, "PowerOff")
		default:
			return fmt.Errorf("power: want on or off, got %q", args[0])
		}

	case "play":
		if len(args) == 0 {
			return fmt.Errorf("play needs a file")
		}
		return cmdPlay(ctx, c, d, args[0])
	}
	return fmt.Errorf("unknown command %q (try: roku help)", cmd)
}

// parseInterspersed parses flags that may appear before or after the
// positional arguments.
//
// The flag package stops at the first non-flag argument, so
// "serve ~/Pictures --port 8200" would silently ignore --port. Re-parsing
// what is left after each positional accepts either order, which is what
// anyone typing a command actually expects.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func cmdDiscover(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("discover", flag.ExitOnError)
	save := fs.Bool("save", false, "record what we find in the device registry")
	wait := fs.Duration("wait", 3*time.Second, "how long to listen for replies")
	if err := fs.Parse(args); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "searching for %s...\n", *wait)
	found, err := roku.Discover(ctx, *wait)
	if err != nil {
		return err
	}
	if len(found) == 0 {
		return fmt.Errorf("no Rokus answered.\n" +
			"  - check the TV is on the same network (not a guest SSID)\n" +
			"  - Settings > System > Advanced system settings > Control by mobile apps must be Enabled\n" +
			"  - a TV asleep without Fast TV Start will not answer")
	}
	printDevices(found)
	if *save {
		reg, err := roku.LoadRegistry()
		if err != nil {
			return err
		}
		reg.Merge(found)
		if err := reg.Save(); err != nil {
			return err
		}
		path, _ := roku.RegistryPath()
		fmt.Fprintf(os.Stderr, "\nsaved %d device(s) to %s\n", len(found), path)
	}
	return nil
}

// cmdScan sweeps the subnet directly, for the case where multicast does
// not make it between the machine and the TVs.
func cmdScan(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	save := fs.Bool("save", false, "record what we find in the device registry")
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	var cidr string
	if len(rest) > 0 {
		cidr = rest[0]
	}
	if cidr == "" {
		var err error
		if cidr, err = roku.LocalCIDR(); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "sweeping %s for ECP responders...\n", cidr)
	found, err := roku.Scan(ctx, cidr, 2*time.Second)
	if err != nil {
		return err
	}
	if len(found) == 0 {
		return fmt.Errorf("nothing on port 8060 in %s", cidr)
	}
	printDevices(found)
	if *save {
		reg, err := roku.LoadRegistry()
		if err != nil {
			return err
		}
		reg.Merge(found)
		if err := reg.Save(); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "\nsaved %d device(s)\n", len(found))
	}
	return nil
}

// cmdServe runs a DLNA media server until interrupted.
//
// This is the working route for local files: ECP cannot hand a Roku a URL
// on current firmware, but Roku Media Player still browses DLNA servers.
func cmdServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	name := fs.String("name", "", "name shown in the TV's media server list")
	port := fs.Int("port", 0, "HTTP port (0 picks a free one)")
	quiet := fs.Bool("quiet", false, "do not log renderer requests")
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	var dir string
	if len(rest) > 0 {
		dir = rest[0]
	}
	if dir == "" {
		var err error
		if dir, err = os.Getwd(); err != nil {
			return err
		}
	}

	srv, err := dlna.NewServer(dir, *name)
	if err != nil {
		return err
	}
	if !*quiet {
		srv.Logf = func(format string, a ...any) {
			fmt.Printf("  "+format+"\n", a...)
		}
	}
	if err := srv.Start(*port); err != nil {
		return err
	}

	folders, files := srv.Library.Count()
	fmt.Printf("serving %s as %q\n", srv.Library.Root, srv.FriendlyName)
	fmt.Printf("  %d folders, %d media files\n", folders, files)
	fmt.Printf("  http://%s/device.xml\n", srv.Addr())
	fmt.Printf("\nOn the TV: Roku Media Player > Media Servers > %s\n", srv.FriendlyName)
	fmt.Println("Ctrl-C to stop.")

	// Run blocks until the context is cancelled, then withdraws the SSDP
	// advertisement so the TV stops offering a server that has gone away.
	return srv.Run(ctx)
}

// cmdMacro manages saved key sequences. These are global rather than
// per-device: the same sequence usually works on every TV in the house.
func cmdMacro(args []string) error {
	ms, err := roku.LoadMacros()
	if err != nil {
		return err
	}
	sub := "list"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}

	switch sub {
	case "list":
		names := ms.Names()
		if len(names) == 0 {
			path, _ := roku.MacroPath()
			fmt.Printf("no macros yet (they live in %s)\n", path)
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		for _, n := range names {
			m, _ := ms.Get(n)
			desc := m.Description
			if desc == "" {
				desc = strings.Join(m.Steps, "; ")
			}
			fmt.Fprintf(w, "  %s\t%s\n", n, desc)
		}
		return w.Flush()

	case "show":
		if len(args) == 0 {
			return fmt.Errorf("macro show needs a name")
		}
		m, ok := ms.Get(args[0])
		if !ok {
			return fmt.Errorf("no macro named %q", args[0])
		}
		fmt.Printf("%s\n", args[0])
		if m.Description != "" {
			fmt.Printf("  %s\n", m.Description)
		}
		for i, step := range m.Steps {
			fmt.Printf("  %d. %s\n", i+1, step)
		}
		return nil

	case "set":
		fs := flag.NewFlagSet("macro set", flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		desc := fs.String("description", "", "what this macro is for")
		rest, err := parseInterspersed(fs, args)
		if err != nil {
			return err
		}
		if len(rest) < 2 {
			return fmt.Errorf(`macro set needs a name and steps, e.g.:` + "\n" +
				`  roku macro set yt "launch youtube contentID=$1; select-until-playing"`)
		}
		steps := roku.ParseSteps(strings.Join(rest[1:], " "))
		if err := ms.Set(rest[0], roku.Macro{Description: *desc, Steps: steps}); err != nil {
			return err
		}
		if err := ms.Save(); err != nil {
			return err
		}
		fmt.Printf("saved macro %q (%d steps)\n", rest[0], len(steps))
		return nil

	case "rm", "remove", "delete":
		if len(args) == 0 {
			return fmt.Errorf("macro rm needs a name")
		}
		if err := ms.Remove(args[0]); err != nil {
			return err
		}
		if err := ms.Save(); err != nil {
			return err
		}
		fmt.Printf("removed macro %q\n", args[0])
		return nil
	}
	return fmt.Errorf("unknown macro command %q (want list, show, set or rm)", sub)
}

func cmdList() error {
	reg, err := roku.LoadRegistry()
	if err != nil {
		return err
	}
	if len(reg.Devices) == 0 {
		return fmt.Errorf("no saved devices; run `roku discover --save`")
	}
	printDevices(reg.Devices)
	return nil
}

func cmdName(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("name needs the current name and the new one")
	}
	reg, err := roku.LoadRegistry()
	if err != nil {
		return err
	}
	if err := reg.Rename(args[0], args[1]); err != nil {
		return err
	}
	if err := reg.Save(); err != nil {
		return err
	}
	fmt.Printf("%s is now %q\n", args[0], args[1])
	return nil
}

// cmdPlay serves a local file and points the TV at it, holding the server
// open until playback ends or the user interrupts.
func cmdPlay(ctx context.Context, c *roku.Client, d roku.Device, path string) error {
	stream, err := roku.ServeFile(path, d.Addr)
	if err != nil {
		return err
	}
	defer stream.Close()

	kind, format := roku.KindFor(stream.Name)
	fmt.Printf("%s: serving %s at %s\n", d.Name, stream.Name, stream.URL)
	if err := c.PlayURL(ctx, stream.URL, stream.Name, kind, format); err != nil {
		return fmt.Errorf("telling %s to play: %w", d.Name, err)
	}

	// The TV pulls the file from us, so we have to stay alive. Watch the
	// player and stop once it closes; Ctrl-C works at any point.
	fmt.Println("streaming — Ctrl-C to stop")
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	started := false
	for {
		select {
		case <-ctx.Done():
			fmt.Println("\nstopped serving")
			return nil
		case <-ticker.C:
			m, err := c.MediaPlayer(context.WithoutCancel(ctx))
			if err != nil {
				continue // transient; keep serving
			}
			if m.Playing() {
				started = true
				continue
			}
			if started {
				fmt.Println("playback finished")
				return nil
			}
		}
	}
}

// eachDevice resolves and runs fn against every target, concurrently, and
// reports every failure rather than stopping at the first.
func eachDevice(ctx context.Context, reg *roku.Registry, targets []roku.Device, fn func(context.Context, *roku.Client, roku.Device) error) error {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, t := range targets {
		wg.Add(1)
		go func(t roku.Device) {
			defer wg.Done()
			c, d, err := reg.Resolve(ctx, t)
			if err == nil {
				err = fn(ctx, c, d)
			}
			if err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s: %w", t.Name, err))
				mu.Unlock()
			}
		}(t)
	}
	wg.Wait()
	if len(errs) == 0 {
		return nil
	}
	var b strings.Builder
	for i, e := range errs {
		if i > 0 {
			b.WriteString("\n       ")
		}
		b.WriteString(e.Error())
	}
	return fmt.Errorf("%s", b.String())
}

func printDevices(ds []roku.Device) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tADDRESS\tMODEL\tSERIAL\tTYPE")
	for _, d := range ds {
		kind := "device"
		if d.IsTV {
			kind = "TV"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", d.Name, d.Addr, d.Model, d.Serial, kind)
	}
	w.Flush()
}

// printCaps lays out what the TV will and will not allow, and says how to
// unlock the rest, since the fix is a menu on the TV and not anything we
// can do over the network.
// printProfiles lists the channels that have an account tile configured.
func printProfiles(ctx context.Context, c *roku.Client, d roku.Device) error {
	names := map[string]string{}
	if apps, err := c.Apps(ctx); err == nil {
		for _, a := range apps {
			names[a.ID] = a.Name
		}
	}
	configured := map[string]int{}
	for id, n := range d.Profiles {
		configured[id] = n
	}
	if n := d.Profile(roku.AppYouTube); n > 0 {
		configured[roku.AppYouTube] = n
	}
	if len(configured) == 0 {
		fmt.Printf("%s: no account tiles configured\n", d.Name)
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "%s\n", d.Name)
	for id, n := range configured {
		label := names[id]
		if label == "" {
			label = "app " + id
		}
		fmt.Fprintf(w, "  %s\ttile %d\n", label, n)
	}
	return w.Flush()
}

func printCaps(d roku.Device, c roku.Capabilities) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "%s\t%s\n", d.Name, c.Summary())
	row := func(label string, ok bool) {
		mark := "blocked"
		if ok {
			mark = "ok"
		}
		fmt.Fprintf(w, "  %s\t%s\n", label, mark)
	}
	row("volume / power", c.Volume)
	row("launch channels (YouTube)", c.Launch)
	row("navigation keys", c.Navigate)
	row("channel list", c.Apps)
	row("playback status", c.Playback)
	row("stream local files", c.Streaming)
	row("airplay receiver", c.AirPlay)
	row("developer mode (UI inspection)", c.Developer)
	w.Flush()
	if c.Permissive && !c.Streaming {
		fmt.Printf("\n  %s reports has-play-on-roku=false: current firmware\n"+
			"  dropped the receiver that accepted a URL, so `roku play` cannot\n"+
			"  work over ECP on this device regardless of access mode.\n", d.Name)
	}
	if !c.Permissive {
		fmt.Printf("\n  To unlock the rest, on %s:\n"+
			"    Settings > System > Advanced system settings >\n"+
			"      Control by mobile apps > Network access > Permissive\n", d.Name)
	}
	return nil
}

func printInfo(d roku.Device, i *roku.DeviceInfo) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "%s\n", d.Name)
	fmt.Fprintf(w, "  address\t%s\n", d.Addr)
	fmt.Fprintf(w, "  model\t%s (%s)\n", i.ModelName, i.ModelNumber)
	fmt.Fprintf(w, "  serial\t%s\n", i.SerialNumber)
	fmt.Fprintf(w, "  software\t%s\n", i.SoftwareVersion)
	fmt.Fprintf(w, "  power\t%s\n", i.PowerMode)
	fmt.Fprintf(w, "  network\t%s %s\n", i.NetworkType, i.NetworkName)
	return w.Flush()
}

func printStatus(ctx context.Context, c *roku.Client, d roku.Device) error {
	app, err := c.ActiveApp(ctx)
	if err != nil {
		return err
	}
	line := d.Name + ": " + app.Name
	if m, err := c.MediaPlayer(ctx); err == nil && m.Playing() {
		line += fmt.Sprintf(" — %s", m.State)
		if m.Position != "" {
			line += fmt.Sprintf(" %s", m.Position)
			if m.Duration != "" {
				line += " / " + m.Duration
			}
		}
	}
	fmt.Println(line)
	return nil
}
