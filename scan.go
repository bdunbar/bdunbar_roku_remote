package roku

import (
	"context"
	"net"
	"sort"
	"sync"
	"time"
)

// Scan probes every address on the given IPv4 network for an ECP responder.
//
// This is the fallback for when SSDP comes back short: multicast is
// routinely dropped between wireless bands and access points, while a
// direct TCP connection to port 8060 is not. It is slower than Discover
// but it does not miss devices that are listening.
func Scan(ctx context.Context, cidr string, timeout time.Duration) ([]Device, error) {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	ips, err := hostsIn(cidr)
	if err != nil {
		return nil, err
	}

	var (
		mu   sync.Mutex
		out  []Device
		wg   sync.WaitGroup
		gate = make(chan struct{}, 64) // bound concurrency; 254 dials at once is rude
	)
	for _, ip := range ips {
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			select {
			case gate <- struct{}{}:
				defer func() { <-gate }()
			case <-ctx.Done():
				return
			}

			addr := NormalizeAddr(ip)
			// Cheap reject first: most addresses have nothing on 8060.
			conn, err := net.DialTimeout("tcp", addr, timeout)
			if err != nil {
				return
			}
			conn.Close()

			probeCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			info, err := NewClient(addr).DeviceInfo(probeCtx)
			if err != nil {
				return // something else is on 8060; not a Roku
			}
			d := Device{Addr: addr, Name: addr}
			d.ApplyInfo(info)
			mu.Lock()
			out = append(out, d)
			mu.Unlock()
		}(ip)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// LocalCIDR reports the IPv4 network this machine sits on, so `roku scan`
// needs no arguments in the common case.
func LocalCIDR() (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok || n.IP.To4() == nil {
				continue
			}
			return (&net.IPNet{IP: n.IP.Mask(n.Mask), Mask: n.Mask}).String(), nil
		}
	}
	return "", net.UnknownNetworkError("no usable IPv4 interface")
}

// hostsIn expands a CIDR into its usable host addresses, skipping the
// network and broadcast addresses.
func hostsIn(cidr string) ([]string, error) {
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, err
	}
	var ips []string
	for ip := n.IP.Mask(n.Mask).To4(); ip != nil && n.Contains(ip); ip = nextIP(ip) {
		ips = append(ips, ip.String())
	}
	if len(ips) > 2 {
		ips = ips[1 : len(ips)-1] // drop network and broadcast
	}
	return ips, nil
}

func nextIP(ip net.IP) net.IP {
	out := make(net.IP, len(ip))
	copy(out, ip)
	for i := len(out) - 1; i >= 0; i-- {
		out[i]++
		if out[i] != 0 {
			return out
		}
	}
	return nil // wrapped past the end of the address space
}
