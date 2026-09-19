# roku

A remote control for Roku TVs on your local network, in Go, with no dependencies
outside the standard library.

Roku devices expose ECP (External Control Protocol) on TCP port 8060: plain
HTTP, no authentication, no handshake. Find the TV, POST to it.

```
roku discover --save
roku name "Living Room TV" living
roku --tv living vol +5
roku --tv living youtube https://youtu.be/dQw4w9WgXcQ
roku serve ~/Videos
roku --tv all power off
```

## Install

```
go build -o roku ./cmd/roku
```

## Finding your TVs

`roku discover` uses SSDP multicast, which is fast but is routinely dropped
between wireless bands and access points. If it comes up short, sweep the
subnet directly:

```
roku scan --save
```

Devices are remembered in `~/.config/roku/devices.json`, keyed by serial
number rather than address, so a TV that gets a new DHCP lease is found again
and keeps whatever alias you gave it.

## Network access modes — read this first

Roku firmware 12.5 and later ships with **Control by mobile apps → Network
access** set to `Default`, which permits only part of ECP. Run:

```
roku --tv all check
```

| | `Disabled` | `Default` | `Permissive` |
|---|---|---|---|
| port 8060 open | ✗ | ✓ | ✓ |
| device info, active app | ✗ | ✓ | ✓ |
| volume, mute, power | ✗ | ✓ | ✓ |
| launch channels (YouTube) | ✗ | ✓ | ✓ |
| navigation keys (Home, arrows, Select) | ✗ | ✗ | ✓ |
| channel list, playback status | ✗ | ✗ | ✓ |
| streaming local files | ✗ | ✗ | see below |

To unlock everything, on each TV:

> Settings → System → Advanced system settings →
> Control by mobile apps → Network access → **Permissive**

This cannot be changed over the network — that restriction is the whole point
of it. A TV whose setting is `Disabled` will not answer discovery or a port
scan at all, because 8060 is closed.

## Streaming local files does not work over ECP

This is worth stating plainly, because every guide on the internet says it
does. On current firmware (tested against 15.3.4 on TCL 32S335 and 55S515):

- `/input/15985`, the Play on Roku receiver, returns **404**. It is gone.
- The device reports **`has-play-on-roku=false`** in `/query/device-info`.
- Roku Media Player (channel 2213) still installs and still answers
  `/launch/2213` with **200** — but it ignores `contentID`/`mediaType`,
  `t`/`u`/`photoName`/`videoFormat`, and every other documented deep-link
  form. Verified from the serving side's access log: the TV opens the
  channel and **never requests the file**.

So a 200 from `/launch/2213` means "channel opened", not "media playing".
`roku check` reads the `has-play-on-roku` flag and reports streaming as
blocked; `roku play` fails fast with `ErrNoPlayOnRoku` rather than launching
a channel that looks like success.

So local files go over DLNA instead. See below.

Two other routes exist and are not implemented here: a sideloaded developer
channel (`/launch/dev?u=<url>`, best experience but needs Developer Mode
enabled per TV), and AirPlay (these TVs report `supports-airplay=true`, but
AirPlay 2 pairing is not a stdlib-only proposition).

## Macros

ECP is write-only. The TV will tell you which channel is running, and
whether something is playing, but nothing about what is on screen or where
the focus is — `/query/sgnodes`, which returns the actual UI node tree, is
gated behind Developer Mode, and enabling that marks the device as a
developer unit and can break DRM-protected apps. Not a trade worth making
on a television anyone actually watches.

So anything past "launch a channel" is a key sequence worked out by looking
at the television. Macros make that a one-time cost:

```
roku macro set yt "launch youtube contentID=$1; select-until-playing 60s"
roku --tv living run yt dQw4w9WgXcQ
```

Steps are separated by semicolons:

```
launch <name|id> [k=v ...]   open a channel, optionally with parameters
<Key> [count]                send a remote key (or: press <Key> [count])
text <words...>              type into the focused field
wait <duration>              pause
wait-playing [duration]      wait until something is playing
select-until-playing [dur]   press Select until something plays
```

`$1` to `$9` and `$*` take the arguments given to `run`, so one macro serves
many videos or searches. Steps are validated when the macro is saved rather
than halfway through a replay.

`select-until-playing` is the useful one, and the reason macros are not just
a list of keypresses: it retries against `/query/media-player` instead of
guessing at a delay. Where a signal exists, use it. Where none exists — a
channel whose picker leads to a browse screen rather than to playback — a
macro can only be a fixed `wait` and a press, and cannot confirm anything.

Manage them with `roku macro list`, `roku macro show NAME` and
`roku macro rm NAME`; they live in `~/.config/roku/macros.json`.

## The YouTube account picker

On a TV with several YouTube accounts, launching a video lands on an account
picker and waits. `roku profile` records which tile to choose, and `youtube`
then handles it:

```
roku --tv living profile 1     # 1 = whichever tile the picker has focused
roku --tv living youtube https://youtu.be/dQw4w9WgXcQ
```

Cold start to playing video is about 25 seconds, most of which is YouTube
loading.

Three things this had to get right, none of them obvious:

- **The position is relative, not absolute.** The tile row wraps, so from the
  leftmost tile a Left press jumps to the far right and no sequence of
  presses reliably reaches a fixed position. The picker focuses whoever used
  the TV last, so `1` — select what is already focused — is almost always
  the right answer on a TV you mostly use yourself.
- **The picker does not appear on a schedule.** A cold YouTube start sits on
  a splash screen for longer than the channel takes to "launch", and a
  Select sent into that splash is swallowed. So there is no correct fixed
  delay; `youtube` retries until `/query/media-player` reports playback.
- **A stray Select pauses the video.** If playback starts in the gap between
  a poll and the next press, that press lands on a playing video and pauses
  it. The state is re-checked immediately before every press, and a pause
  caused this way is resumed and confirmed.

The deep link survives the picker: the `contentID` given at launch still
plays once an account is chosen.

Use `--profile 0` to leave the picker alone.

## Sharing local files over DLNA

`roku serve` runs a UPnP/DLNA MediaServer over a directory. Roku Media Player
finds it by itself and browses it — no setup on the TV at all.

```
roku serve ~/Videos
roku serve ~/Pictures --name "hilda pics" --port 8200
```

Then on the TV: **Roku Media Player → the server's name → your files**.
`roku launch 2213` opens Roku Media Player from the command line.

What the server does:

- **SSDP** (`ssdp.go`) — answers `M-SEARCH` probes *and* sends unsolicited
  `ssdp:alive` announcements. Both are needed: renderers already running
  learn about us from the announcements, renderers that start later find us
  by searching. Implementing only one is the usual reason a media server is
  visible to some devices and not others. On shutdown it sends `ssdp:byebye`
  so the TV stops offering a server that has gone away.
- **ContentDirectory** (`soap.go`) — answers the `Browse` action over SOAP,
  returning DIDL-Lite. Note that DIDL is XML nested *inside* XML, escaped,
  which is why it is built as a string rather than as structs.
- **Media delivery** (`server.go`) — `http.ServeContent`, so byte-range
  requests work and the TV can seek. The `DLNA.ORG_OP=01` flag in
  `protocolInfo` is what tells the renderer seeking is available before it
  tries; images get `OP=00` because they are not seekable.

The server binds to the LAN address specifically rather than `0.0.0.0`.
Advertising a URL on an address the TV cannot reach is the most common way a
DLNA server shows up in the list but plays nothing.

Symlinks are followed when sizing files, so a library of symlinks into other
volumes advertises real sizes rather than the length of each link.

## Commands

```
discover [--save]      find Rokus via SSDP
scan [--save] [CIDR]   sweep the subnet for port 8060
serve [DIR]            share a directory over DLNA (see above)
list                   show saved devices
name OLD NEW           give a device a shorter alias
check                  report what this TV's access mode allows
info                   device details
status                 what's on screen and what's playing
apps                   installed channels and their IDs
profile [N]            show or set the YouTube account tile to pick
macro list|show|set|rm recorded key sequences
run NAME [ARGS...]     replay a macro on the target TV
vol +N | -N | mute     change volume (Roku TVs only)
key NAME [count]       send a remote key (roku key --list)
text "hello"           type into the focused field
launch NAME|ID         open a channel by name or app ID
youtube ID|URL         play a YouTube video
play FILE              stream a local file to the TV
power on|off           power the TV on or off
```

`--tv` takes an alias, a serial, a bare IP, or `all` (the default). It also
reads `$ROKU_TV`. Commands against `all` run concurrently and report every
failure rather than stopping at the first.

## Layout

The split exists so that a GUI is a second `main`, not a rewrite:

```
roku/            the library: protocol only, no printing, no os.Exit,
                 context.Context on everything that touches the network
  device.go        client, keys, volume, launch
  discover.go      SSDP
  scan.go          subnet sweep fallback
  query.go         XML replies: device-info, apps, active-app, media-player
  media.go         YouTube, local file server, play-on-roku detection
  access.go        access-mode detection and the 403 explanation
  registry.go      saved devices, aliases, serial-based healing
dlna/            UPnP/DLNA MediaServer, also dependency-free
  library.go       directory -> content tree
  didl.go          DIDL-Lite metadata
  soap.go          ContentDirectory Browse over SOAP
  descriptors.go   device.xml and the service descriptions
  ssdp.go          multicast presence: search replies and announcements
  server.go        HTTP service and media delivery
cmd/roku/        the CLI: argument parsing, printing, exit codes
```

Nothing in the `roku` package knows what a terminal is. A GUI imports it,
calls `Discover` and `Probe` on startup, and wires buttons to `Press`,
`Volume` and `PlayYouTube`.

## Notes

- ECP is unauthenticated. Anything on your LAN can control these TVs.
- `/launch` blocks until the channel has loaded and can outlive a short HTTP
  timeout. `Launch` confirms against `/query/active-app` before reporting a
  timeout as a failure.
- Volume and power apply to Roku *TVs*; sticks and boxes have no volume of
  their own.
- `power on` needs Fast TV Start enabled, or the network stack is asleep and
  nothing answers.
- Amazon Fire TV devices do **not** speak ECP. They are a different protocol
  (ADB on port 5555) and are out of scope here.
