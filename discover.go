package roku

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	ssdpAddr   = "239.255.255.250:1900"
	ssdpTarget = "roku:ecp"
)

// The SSDP search datagram. Note the quoted Man header and CRLF line endings
// with a blank line at the end — Roku is picky about both.
var msearch = strings.Join([]string{
	"M-SEARCH * HTTP/1.1",
	"Host: " + ssdpAddr,
	`Man: "ssdp:discover"`,
	"ST: " + ssdpTarget,
	"MX: 3",
	"",
	"",
}, "\r\n")

// Discover finds every Roku that answers an SSDP search within wait.
//
// Discovery is UDP multicast, so it is best-effort: the search is sent
// several times because datagrams get dropped, and a device that is asleep
// with Fast TV Start disabled will not answer at all.
func Discover(ctx context.Context, wait time.Duration) ([]Device, error) {
	if wait <= 0 {
		wait = 3 * time.Second
	}
	raddr, err := net.ResolveUDPAddr("udp4", ssdpAddr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// Three shots, spaced out, to survive ordinary multicast loss.
	go func() {
		for i := 0; i < 3; i++ {
			if _, err := conn.WriteToUDP([]byte(msearch), raddr); err != nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(250 * time.Millisecond):
			}
		}
	}()

	// Collect distinct LOCATION headers first and only then ask each device
	// who it is, so that slow HTTP replies never cost us a UDP response.
	found := map[string]string{} // addr -> serial from USN
	deadline := time.Now().Add(wait)
	_ = conn.SetReadDeadline(deadline)
	buf := make([]byte, 4096)
	for {
		if ctx.Err() != nil {
			break
		}
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			break // read deadline: we are done listening
		}
		loc, usn := parseSSDP(buf[:n])
		if loc == "" {
			continue
		}
		addr := addrFromLocation(loc)
		if addr == "" {
			continue
		}
		if _, dup := found[addr]; !dup {
			found[addr] = serialFromUSN(usn)
		}
	}

	return describeAll(ctx, found), nil
}

// describeAll turns raw addresses into populated Devices, querying them
// concurrently since four TVs should not take four round trips.
func describeAll(ctx context.Context, found map[string]string) []Device {
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		out []Device
	)
	for addr, serial := range found {
		wg.Add(1)
		go func(addr, serial string) {
			defer wg.Done()
			d := Device{Addr: addr, Serial: serial, Name: addr}
			if info, err := NewClient(addr).DeviceInfo(ctx); err == nil {
				d.ApplyInfo(info)
			}
			mu.Lock()
			out = append(out, d)
			mu.Unlock()
		}(addr, serial)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// parseSSDP pulls LOCATION and USN out of an SSDP response. The response is
// HTTP-shaped, so we reuse the stdlib parser rather than hand-rolling one.
func parseSSDP(b []byte) (location, usn string) {
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(b)), nil)
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()
	return resp.Header.Get("Location"), resp.Header.Get("Usn")
}

// addrFromLocation converts "http://192.168.1.20:8060/" to "192.168.1.20:8060".
func addrFromLocation(loc string) string {
	u, err := url.Parse(strings.TrimSpace(loc))
	if err != nil || u.Host == "" {
		return ""
	}
	return NormalizeAddr(u.Host)
}

// serialFromUSN extracts the serial from "uuid:roku:ecp:X0012345ABCD".
func serialFromUSN(usn string) string {
	if i := strings.LastIndex(usn, ":"); i >= 0 {
		return strings.TrimSpace(usn[i+1:])
	}
	return ""
}
