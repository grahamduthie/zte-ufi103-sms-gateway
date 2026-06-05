package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"strings"
	"time"
)

var mdnsGroup = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}

// mdnsEncodedName is "dongle.local" in DNS wire format.
var mdnsEncodedName = []byte{6, 'd', 'o', 'n', 'g', 'l', 'e', 5, 'l', 'o', 'c', 'a', 'l', 0}

const mdnsHostname = "dongle.local"

// runMDNS listens for mDNS queries on 224.0.0.251:5353 and responds to A-record
// queries for "dongle.local" with the current wlan0 IP address.
func runMDNS(ctx context.Context, logger *log.Logger) {
	defer func() {
		if r := recover(); r != nil {
			logger.Printf("mDNS: recovered: %v", r)
		}
	}()

	iface := mdnsIface()
	if iface == nil {
		logger.Printf("mDNS: no suitable interface; not starting")
		return
	}

	conn, err := net.ListenMulticastUDP("udp4", iface, mdnsGroup)
	if err != nil {
		logger.Printf("mDNS: listen: %v", err)
		return
	}
	defer conn.Close()

	logger.Printf("mDNS: advertising %s on %s", mdnsHostname, iface.Name)

	buf := make([]byte, 4096)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		conn.SetDeadline(time.Now().Add(time.Second))
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			logger.Printf("mDNS: read: %v", err)
			continue
		}

		ip := mdnsIfaceIP(iface)
		if ip == nil {
			continue
		}

		resp, ok := buildMDNSResponse(buf[:n], ip)
		if !ok {
			continue
		}
		if _, err := conn.WriteToUDP(resp, mdnsGroup); err != nil {
			logger.Printf("mDNS: write: %v", err)
		}
	}
}

func mdnsIface() *net.Interface {
	if iface, err := net.InterfaceByName("wlan0"); err == nil {
		return iface
	}
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		i := i
		if i.Flags&(net.FlagMulticast|net.FlagUp) == (net.FlagMulticast|net.FlagUp) &&
			i.Flags&net.FlagLoopback == 0 {
			return &i
		}
	}
	return nil
}

func mdnsIfaceIP(iface *net.Interface) net.IP {
	addrs, _ := iface.Addrs()
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok {
			if ip4 := ipnet.IP.To4(); ip4 != nil {
				return ip4
			}
		}
	}
	return nil
}

// buildMDNSResponse parses a DNS query and returns a multicast mDNS A-record
// response for mdnsHostname, or nil/false if the query doesn't match.
func buildMDNSResponse(pkt []byte, ip net.IP) ([]byte, bool) {
	if len(pkt) < 12 || pkt[2]&0x80 != 0 { // must be a query (QR=0)
		return nil, false
	}

	id := binary.BigEndian.Uint16(pkt[0:2])
	qdCount := int(binary.BigEndian.Uint16(pkt[4:6]))

	offset := 12
	found := false
	for i := 0; i < qdCount; i++ {
		name, next, err := parseDNSName(pkt, offset)
		if err != nil || next+4 > len(pkt) {
			return nil, false
		}
		qtype := binary.BigEndian.Uint16(pkt[next : next+2])
		offset = next + 4
		if strings.EqualFold(name, mdnsHostname) && (qtype == 1 /* A */ || qtype == 255 /* ANY */) {
			found = true
		}
	}
	if !found {
		return nil, false
	}

	ip4 := ip.To4()
	if ip4 == nil {
		return nil, false
	}

	var resp []byte
	resp = binary.BigEndian.AppendUint16(resp, id)
	resp = binary.BigEndian.AppendUint16(resp, 0x8400) // QR=1, AA=1
	resp = binary.BigEndian.AppendUint16(resp, 0)      // QDCount (no question section in mDNS responses)
	resp = binary.BigEndian.AppendUint16(resp, 1)      // ANCount
	resp = binary.BigEndian.AppendUint16(resp, 0)      // NSCount
	resp = binary.BigEndian.AppendUint16(resp, 0)      // ARCount
	resp = append(resp, mdnsEncodedName...)
	resp = binary.BigEndian.AppendUint16(resp, 1)      // TYPE A
	resp = binary.BigEndian.AppendUint16(resp, 0x8001) // CLASS IN | cache-flush
	resp = binary.BigEndian.AppendUint32(resp, 120)    // TTL 120s
	resp = binary.BigEndian.AppendUint16(resp, 4)      // RDLENGTH
	resp = append(resp, ip4...)
	return resp, true
}

// parseDNSName reads a DNS name from pkt at offset, following compression
// pointers. Returns the lowercase dot-joined name and the offset of the byte
// immediately after the name (before any pointer target).
func parseDNSName(pkt []byte, offset int) (string, int, error) {
	var parts []string

	for {
		if offset >= len(pkt) {
			return "", 0, fmt.Errorf("truncated name")
		}
		b := int(pkt[offset])
		if b == 0 {
			offset++
			break
		}
		if b&0xC0 == 0xC0 { // compression pointer
			if offset+1 >= len(pkt) {
				return "", 0, fmt.Errorf("truncated pointer")
			}
			end := offset + 2
			ptr := (b&0x3F)<<8 | int(pkt[offset+1])
			rest, _, err := parseDNSName(pkt, ptr)
			if err != nil {
				return "", 0, err
			}
			if rest != "" {
				parts = append(parts, rest)
			}
			return strings.Join(parts, "."), end, nil
		}
		offset++
		if offset+b > len(pkt) {
			return "", 0, fmt.Errorf("label out of bounds")
		}
		parts = append(parts, strings.ToLower(string(pkt[offset:offset+b])))
		offset += b
	}

	return strings.Join(parts, "."), offset, nil
}
