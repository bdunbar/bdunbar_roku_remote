package dlna

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"strings"
	"time"
)

const (
	ssdpAddr    = "239.255.255.250:1900"
	ssdpMaxAge  = 1800
	notifyEvery = 300 * time.Second
	serverID    = "Linux/1.0 UPnP/1.0 roku-cli/1.0"
)

// ssdpResponder is the server's presence on the network: it answers
// M-SEARCH probes and periodically announces itself unprompted.
//
// Both halves are needed. A renderer that is already running discovers us
// from the NOTIFY announcements; one that starts later finds us by
// searching. Implementing only one is the usual reason a media server is
// "invisible" to half the devices in a house.
type ssdpResponder struct {
	udn      string
	location string
	conn     *net.UDPConn // joined to the multicast group, for receiving
	out      *net.UDPConn // unicast socket, for replies and announcements
	target   *net.UDPAddr
}

// targets are the search targets we answer to, and the ones we announce
// under. A renderer may search for any of them.
func (s *ssdpResponder) targets() []string {
	return []string{rootDeviceType, s.udn, deviceType, cdsType, cmsType}
}

// usnFor builds the Unique Service Name pairing our device with a target.
func (s *ssdpResponder) usnFor(target string) string {
	if target == s.udn {
		return s.udn
	}
	return s.udn + "::" + target
}

func newSSDPResponder(udn, location string) (*ssdpResponder, error) {
	target, err := net.ResolveUDPAddr("udp4", ssdpAddr)
	if err != nil {
		return nil, err
	}
	// Joining the group on all interfaces (nil) is what lets us hear
	// searches regardless of which interface the renderer is on.
	conn, err := net.ListenMulticastUDP("udp4", nil, target)
	if err != nil {
		return nil, fmt.Errorf("joining the SSDP multicast group: %w", err)
	}
	out, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetReadBuffer(1 << 20)
	return &ssdpResponder{udn: udn, location: location, conn: conn, out: out, target: target}, nil
}

// run serves SSDP until the context is cancelled, then says goodbye.
func (s *ssdpResponder) run(ctx context.Context) {
	go s.listen(ctx)
	go s.announce(ctx)
	<-ctx.Done()
	s.byebye()
	s.conn.Close()
	s.out.Close()
}

// listen answers M-SEARCH probes aimed at anything we provide.
func (s *ssdpResponder) listen(ctx context.Context) {
	buf := make([]byte, 2048)
	for {
		if ctx.Err() != nil {
			return
		}
		s.conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, from, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			continue // almost always the read deadline
		}
		req := string(buf[:n])
		if !strings.HasPrefix(req, "M-SEARCH") {
			continue // a NOTIFY from some other device
		}
		st := headerValue(req, "ST")
		mx := headerValue(req, "MX")

		for _, target := range s.targets() {
			if st != "ssdp:all" && st != target {
				continue
			}
			// The spec asks responders to wait a random interval up to MX
			// so that a houseful of devices does not answer in unison.
			go s.respondAfter(jitter(mx), from, target)
		}
	}
}

func (s *ssdpResponder) respondAfter(d time.Duration, to *net.UDPAddr, target string) {
	time.Sleep(d)
	resp := "HTTP/1.1 200 OK\r\n" +
		fmt.Sprintf("CACHE-CONTROL: max-age=%d\r\n", ssdpMaxAge) +
		"DATE: " + time.Now().UTC().Format(time.RFC1123) + "\r\n" +
		"EXT:\r\n" +
		"LOCATION: " + s.location + "\r\n" +
		"SERVER: " + serverID + "\r\n" +
		"ST: " + target + "\r\n" +
		"USN: " + s.usnFor(target) + "\r\n" +
		"BOOTID.UPNP.ORG: 1\r\nCONFIGID.UPNP.ORG: 1\r\n\r\n"
	s.out.WriteToUDP([]byte(resp), to)
}

// announce sends unsolicited ssdp:alive notifications, repeated well inside
// the advertised max-age so our entry never expires from a renderer's list.
func (s *ssdpResponder) announce(ctx context.Context) {
	send := func() {
		for _, target := range s.targets() {
			msg := "NOTIFY * HTTP/1.1\r\n" +
				"HOST: " + ssdpAddr + "\r\n" +
				fmt.Sprintf("CACHE-CONTROL: max-age=%d\r\n", ssdpMaxAge) +
				"LOCATION: " + s.location + "\r\n" +
				"NT: " + target + "\r\n" +
				"NTS: ssdp:alive\r\n" +
				"SERVER: " + serverID + "\r\n" +
				"USN: " + s.usnFor(target) + "\r\n" +
				"BOOTID.UPNP.ORG: 1\r\nCONFIGID.UPNP.ORG: 1\r\n\r\n"
			s.out.WriteToUDP([]byte(msg), s.target)
			time.Sleep(20 * time.Millisecond)
		}
	}
	// Announce three times on startup; multicast loss is routine.
	for i := 0; i < 3; i++ {
		send()
		select {
		case <-ctx.Done():
			return
		case <-time.After(300 * time.Millisecond):
		}
	}
	ticker := time.NewTicker(notifyEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			send()
		}
	}
}

// byebye withdraws our advertisements so renderers drop us promptly instead
// of offering a dead server for the next half hour.
func (s *ssdpResponder) byebye() {
	for _, target := range s.targets() {
		msg := "NOTIFY * HTTP/1.1\r\n" +
			"HOST: " + ssdpAddr + "\r\n" +
			"NT: " + target + "\r\n" +
			"NTS: ssdp:byebye\r\n" +
			"USN: " + s.usnFor(target) + "\r\n\r\n"
		s.out.WriteToUDP([]byte(msg), s.target)
	}
}

// headerValue pulls one header out of an SSDP message. Header names are
// case-insensitive here and devices genuinely vary in how they spell them.
func headerValue(msg, name string) string {
	for _, line := range strings.Split(msg, "\r\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(k), name) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// jitter converts an MX header into a random delay, clamped to sane bounds.
func jitter(mx string) time.Duration {
	secs := 2
	if mx != "" {
		if n, err := fmt.Sscanf(mx, "%d", &secs); n != 1 || err != nil {
			secs = 2
		}
	}
	if secs < 1 {
		secs = 1
	}
	if secs > 5 {
		secs = 5
	}
	return time.Duration(rand.Int63n(int64(secs) * int64(time.Second)))
}
