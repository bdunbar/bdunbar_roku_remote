package roku

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// YouTube's channel ID is the same on every Roku.
const AppYouTube = "837"

// playOnRoku is the built-in receiver that the phone app's "Play on Roku"
// feature targets. Older firmware prefers the Roku Media Player channel
// instead, so PlayURL tries both.
const (
	playOnRoku      = "15985"
	rokuMediaPlayer = "2213"
)

// MediaKind is the t= parameter the receiver uses to pick a player.
type MediaKind string

const (
	Video MediaKind = "v"
	Photo MediaKind = "p"
	Audio MediaKind = "a"
)

// KindFor guesses a media kind and container format from a filename.
func KindFor(name string) (MediaKind, string) {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	switch ext {
	case "mp4", "m4v", "mov":
		return Video, "mp4"
	case "mkv":
		return Video, "mkv"
	case "webm":
		return Video, "mkv" // Roku reports webm under the mkv parser
	case "jpg", "jpeg":
		return Photo, "jpg"
	case "png":
		return Photo, "png"
	case "gif":
		return Photo, "gif"
	case "mp3":
		return Audio, "mp3"
	case "m4a", "aac":
		return Audio, "m4a"
	case "wav":
		return Audio, "wav"
	case "flac":
		return Audio, "flac"
	default:
		return Video, ext
	}
}

// ErrNoPlayOnRoku means the device has no receiver that will accept a URL.
//
// Roku removed the Play on Roku receiver (channel 15985) from current
// firmware; /input/15985 returns 404 and the device reports
// has-play-on-roku=false. Roku Media Player still installs and still
// answers /launch/2213 with 200, but it ignores every documented deep-link
// parameter and never fetches the URL — verified against the access log of
// the serving side. There is no ECP route to playing a local file.
var ErrNoPlayOnRoku = errors.New(
	"this Roku has no Play on Roku receiver (has-play-on-roku=false), " +
		"so ECP cannot hand it a URL to play")

// PlayURL tells the device to fetch and play a URL it can reach itself.
// The URL must be reachable from the TV, not from you — a localhost URL
// will simply do nothing.
//
// Check Capabilities.Streaming first: on current firmware this returns
// ErrNoPlayOnRoku, and a 200 from the fallback launch would be misleading
// because the channel opens without ever playing anything.
func (c *Client) PlayURL(ctx context.Context, mediaURL, name string, kind MediaKind, format string) error {
	q := url.Values{}
	q.Set("t", string(kind))
	q.Set("u", mediaURL)
	q.Set("k", "(null)")
	if name != "" {
		q.Set("videoName", name)
	}
	if format != "" {
		q.Set("videoFormat", format)
	}

	// Hand it to the Play on Roku receiver. On firmware that still has one
	// this works; everywhere else it 404s and there is no substitute, so
	// we report that plainly rather than launching a channel that will sit
	// on its home screen looking like success.
	if err := c.Input(ctx, playOnRoku, q); err != nil {
		return ErrNoPlayOnRoku
	}
	return nil
}

// PlayYouTube opens the YouTube channel on a video. id may be a bare video
// ID or any ordinary YouTube URL.
func (c *Client) PlayYouTube(ctx context.Context, id string) error {
	v, err := YouTubeID(id)
	if err != nil {
		return err
	}
	q := url.Values{}
	q.Set("contentID", v)
	return c.Launch(ctx, AppYouTube, q)
}

// YouTubeID extracts a video ID from the several shapes a link can take.
func YouTubeID(s string) (string, error) {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "/") && !strings.Contains(s, "?") {
		return s, nil // already a bare ID
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("not a YouTube link: %q", s)
	}
	if v := u.Query().Get("v"); v != "" {
		return v, nil
	}
	// youtu.be/ID, /embed/ID, /live/ID, /shorts/ID
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) > 0 {
		last := parts[len(parts)-1]
		if last != "" {
			return last, nil
		}
	}
	return "", fmt.Errorf("could not find a video ID in %q", s)
}

// Stream is a one-file HTTP server, running here, that a TV pulls from.
//
// This is the whole trick behind streaming from the laptop: there is no
// push, no DLNA, no transcoding. We serve the file on the LAN and give the
// TV a URL. http.ServeFile handles range requests, so seeking works.
type Stream struct {
	Path string // the file on disk
	URL  string // what the TV should fetch
	Name string // display name

	ln  net.Listener
	srv *http.Server
}

// ServeFile starts a local HTTP server for path, choosing a local address
// that reachFrom (a device host:port) can actually connect back to.
func ServeFile(path, reachFrom string) (*Stream, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		return nil, fmt.Errorf("%s is a directory", abs)
	}

	ip, err := outboundIP(reachFrom)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", ip.String()+":0")
	if err != nil {
		return nil, err
	}

	name := filepath.Base(abs)
	mux := http.NewServeMux()
	// Serve the single file at any path; some receivers append their own
	// query strings or expect the extension in the URL.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, abs)
	})
	s := &Stream{
		Path: abs,
		Name: name,
		URL:  "http://" + ln.Addr().String() + "/" + url.PathEscape(name),
		ln:   ln,
		srv:  &http.Server{Handler: mux},
	}
	go s.srv.Serve(ln)
	return s, nil
}

// Close stops serving the file.
func (s *Stream) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.srv.Shutdown(ctx)
}

// outboundIP finds which of our addresses the device would see. Dialing a
// UDP socket sends no packets but makes the kernel pick a source address,
// which is exactly the routing decision we want to borrow.
func outboundIP(remote string) (net.IP, error) {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	conn, err := net.Dial("udp", net.JoinHostPort(host, "9"))
	if err != nil {
		return nil, fmt.Errorf("no route to %s: %w", host, err)
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP, nil
}

// parseParams turns "k=v" tokens into query parameters, which is how a
// macro passes a deep link to a channel.
func parseParams(tokens []string) (url.Values, error) {
	if len(tokens) == 0 {
		return nil, nil
	}
	v := url.Values{}
	for _, t := range tokens {
		key, val, ok := strings.Cut(t, "=")
		if !ok {
			return nil, fmt.Errorf("parameter %q must be key=value", t)
		}
		v.Set(key, val)
	}
	return v, nil
}
