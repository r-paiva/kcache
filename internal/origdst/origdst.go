// SPDX-FileCopyrightText: Copyright (c) 2026, the latch developers
//
// SPDX-License-Identifier: Apache-2.0

package origdst

import (
	"encoding/binary"
	"fmt"
	"net"

	"github.com/cilium/ebpf"
)

func Lookup(origDstMap *ebpf.Map, peer *net.TCPAddr) (net.IP, uint16, error) {
	srcIP := peer.IP.To4()
	if srcIP == nil {
		return nil, 0, fmt.Errorf("not an IPv4 address: %v", peer.IP)
	}

	var key struct {
		SrcIP   uint32
		SrcPort uint16
		Pad     uint16
	}
	key.SrcIP = binary.BigEndian.Uint32(srcIP)
	key.SrcPort = uint16(peer.Port)

	var val struct {
		OrigDstIP   uint32
		OrigDstPort uint16
		Pad         uint16
	}
	if err := origDstMap.Lookup(&key, &val); err != nil {
		return nil, 0, fmt.Errorf("orig_dst lookup for %v:%d: %w", peer.IP, peer.Port, err)
	}

	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, val.OrigDstIP)
	return ip, val.OrigDstPort, nil
}
