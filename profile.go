package roku

import (
	"context"
	"fmt"
	"net/url"
	"time"
)

// LaunchOptions controls how a channel launch deals with an account
// picker.
//
// Two completion strategies exist because channels differ in what they do
// once an account is chosen:
//
//   - A channel opened on a deep link starts playing, and playback is
//     observable through /query/media-player. That gives a reliable signal,
//     so the Select can be retried until it lands. This is the YouTube case.
//
//   - A channel opened without one lands on a browse screen and plays
//     nothing. There is no signal, so there is nothing to retry against —
//     and retrying would be actively harmful, since a second Select on a
//     browse screen starts playing whatever happens to be focused. These
//     get one press after a settle delay, and no confirmation is possible.
type LaunchOptions struct {
	// Params are the query parameters for the launch itself.
	Params url.Values
	// ExpectPlayback selects the retry-until-playing strategy.
	ExpectPlayback bool
	// Settle is how long to wait before the single blind press used when
	// ExpectPlayback is false.
	Settle time.Duration
	// Profile is which account tile to choose, counted from the one the
	// picker already has focused: 1 selects the focused tile, 2 the one to
	// its right, and so on. Zero leaves the picker alone.
	//
	// It is relative rather than absolute because the tile row wraps: from
	// the leftmost tile, pressing Left jumps to the far right, so there is
	// no sequence of presses that reliably reaches a fixed position. The
	// picker focuses the account that used the TV last, which for a TV you
	// mostly use yourself means 1 is almost always right.
	Profile int
	// Wait is the total window for the retry strategy.
	Wait time.Duration
}

// YouTubeOptions is the YouTube-shaped subset of LaunchOptions.
type YouTubeOptions struct {
	Profile int
	Wait    time.Duration
}

// LaunchWith opens a channel and deals with an account picker if one is
// configured for it.
func (c *Client) LaunchWith(ctx context.Context, appID string, opt LaunchOptions) error {
	if err := c.Launch(ctx, appID, opt.Params); err != nil {
		return err
	}
	if opt.Profile <= 0 {
		return nil
	}
	if opt.ExpectPlayback {
		return c.selectUntilPlaying(ctx, opt)
	}
	return c.selectAfterSettling(ctx, opt)
}

// selectAfterSettling presses once, blind, for channels whose picker leads
// to a browse screen rather than to playback.
//
// Nothing here can be verified: there is no observable difference between
// a picker that was dismissed and one that was never shown. The press is
// therefore deliberately single — repeating it is what would turn "did
// nothing" into "started playing something random".
func (c *Client) selectAfterSettling(ctx context.Context, opt LaunchOptions) error {
	settle := opt.Settle
	if settle <= 0 {
		settle = 12 * time.Second
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(settle):
	}
	return c.SelectProfile(ctx, opt.Profile)
}

// PlayYouTubeWith opens a video and, if the account picker appears,
// chooses a profile.
//
// The picker cannot be detected directly — ECP reports which channel is
// running, not what it is showing — and it does not appear at a
// predictable moment: a cold YouTube start can sit on a splash screen for
// longer than it takes the channel to "launch", and a Select sent into
// that splash is simply swallowed.
//
// So rather than guess at a delay, this retries: check whether anything is
// playing, press Select if not, and repeat until playback starts or the
// window runs out. Once a video is playing no further keys are sent, so a
// video that starts on its own is never touched.
func (c *Client) PlayYouTubeWith(ctx context.Context, id string, opt YouTubeOptions) error {
	v, err := YouTubeID(id)
	if err != nil {
		return err
	}
	q := url.Values{}
	q.Set("contentID", v)
	return c.LaunchWith(ctx, AppYouTube, LaunchOptions{
		Params:         q,
		Profile:        opt.Profile,
		ExpectPlayback: true,
		Wait:           opt.Wait,
	})
}

// selectUntilPlaying presses Select until something plays.
func (c *Client) selectUntilPlaying(ctx context.Context, opt LaunchOptions) error {
	if opt.Wait <= 0 {
		opt.Wait = 60 * time.Second
	}
	deadline := time.Now().Add(opt.Wait)
	movedToTile := false
	for {
		// Each pass spends a few seconds watching for playback, which also
		// paces the retries without a bare sleep.
		playing, err := c.WaitPlaying(ctx, 4*time.Second)
		if err != nil {
			return err
		}
		if playing {
			m, err := c.MediaPlayer(ctx)
			if err != nil {
				return nil // it is playing; a status hiccup is not a failure
			}
			return c.resumeIfWePaused(ctx, m)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("nothing started playing within %s; if an "+
				"account picker is showing, the tile may be further right "+
				"than profile %d", opt.Wait, opt.Profile)
		}

		// Playback may have begun in the moment between the last poll and
		// now. Pressing Select at a playing video pauses it, so re-check
		// immediately before pressing to keep that window as small as
		// possible.
		if m, err := c.MediaPlayer(ctx); err == nil && m.Playing() {
			return c.resumeIfWePaused(ctx, m)
		}

		// The rightward walk happens once. Repeating it every pass would
		// drift the focus further right with each retry.
		if !movedToTile {
			if opt.Profile > 1 {
				if err := c.PressN(ctx, "Right", opt.Profile-1, 0); err != nil {
					return err
				}
			}
			movedToTile = true
		}
		if err := c.Press(ctx, "Select"); err != nil {
			return err
		}
	}
}

// SelectProfile chooses an account tile, counting rightwards from the one
// the picker already has focused. Index 1 selects the focused tile.
func (c *Client) SelectProfile(ctx context.Context, index int) error {
	if index < 1 {
		return fmt.Errorf("profile position must be 1 or greater, got %d", index)
	}
	if err := c.PressN(ctx, "Right", index-1, 0); err != nil {
		return err
	}
	return c.Press(ctx, "Select")
}

// WaitPlaying reports whether the device starts playing something within
// the given window.
func (c *Client) WaitPlaying(ctx context.Context, within time.Duration) (bool, error) {
	deadline := time.Now().Add(within)
	for {
		m, err := c.MediaPlayer(ctx)
		if err != nil {
			if IsRestricted(err) {
				// Without playback state we cannot tell a picker from a
				// playing video, and guessing would send keypresses into
				// whatever is on screen.
				return false, fmt.Errorf("cannot detect playback: %w", err)
			}
			return false, err
		}
		if m.Playing() {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// resumeIfWePaused restarts playback that one of our own Select presses
// stopped.
//
// The command was asked to play something, so ending on a paused video is
// a failure to deliver even though media is loaded. A stray Select is the
// only way this function is reached with a paused player, so resuming is
// safe here in a way it would not be as a general behaviour.
func (c *Client) resumeIfWePaused(ctx context.Context, m *MediaPlayer) error {
	if !m.Paused() {
		return nil
	}
	if err := c.Press(ctx, "Play"); err != nil {
		return err
	}
	// Confirm rather than assume; if it does not take, say so.
	time.Sleep(1500 * time.Millisecond)
	after, err := c.MediaPlayer(ctx)
	if err != nil {
		return nil
	}
	if after.Paused() {
		return fmt.Errorf("video is loaded but paused, and it did not resume")
	}
	return nil
}
