package tunnel

import (
	"net"
	"strings"
)

// LANAddrs lists this computer's private IPv4 addresses (no loopback,
// link-local or Tailscale 100.64/10 CGNAT addresses).
func LANAddrs() []string {
	var out []string
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		n := strings.ToLower(i.Name)
		if strings.Contains(n, "tailscale") || strings.Contains(n, "vethernet") || strings.HasPrefix(n, "docker") || strings.HasPrefix(n, "br-") || strings.HasPrefix(n, "veth") {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipn.IP.To4()
			if ip == nil || !ip.IsPrivate() {
				continue
			}
			out = append(out, ip.String())
		}
	}
	return out
}

// ListensOnAll reports whether a bind host accepts other computers.
func ListensOnAll(host string) bool {
	h := strings.Trim(strings.TrimSpace(host), "[]")
	return h == "" || h == "0.0.0.0" || h == "::"
}
