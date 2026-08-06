// SPDX-FileCopyrightText: Copyright (c) 2026, the latch developers
//
// SPDX-License-Identifier: Apache-2.0

package podveth

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

const inPodIface = "eth0"

// Resolver maps pod IPs to host veth ifindexes by walking network namespaces
// under procRoot
type Resolver struct {
	procRoot string
}

func NewResolver(procRoot string) *Resolver {
	if procRoot == "" {
		procRoot = "/proc"
	}
	return &Resolver{procRoot: procRoot}
}

// Resolve returns podIP → host veth ifindex for every pod netns on the node
func (r *Resolver) Resolve() (map[string]int, error) {
	entries, err := os.ReadDir(r.procRoot)
	if err != nil {
		return nil, fmt.Errorf("read procfs %s: %w", r.procRoot, err)
	}

	out := make(map[string]int)
	seenInodes := make(map[uint64]struct{})

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		nsPath := filepath.Join(r.procRoot, e.Name(), "ns", "net")
		var st unix.Stat_t
		if err := unix.Stat(nsPath, &st); err != nil {
			continue
		}
		if _, ok := seenInodes[st.Ino]; ok {
			continue
		}
		seenInodes[st.Ino] = struct{}{}
		r.inspectNetns(nsPath, out)
	}
	return out, nil
}

// inspectNetns enters the netns at nsPath, and for its eth0 records
// podIP → host veth ifindex into out.
func (r *Resolver) inspectNetns(nsPath string, out map[string]int) {
	target, err := netns.GetFromPath(nsPath)
	if err != nil {
		return
	}
	defer func() { _ = target.Close() }()

	h, err := netlink.NewHandleAt(target)
	if err != nil {
		return
	}
	defer h.Close()

	link, err := h.LinkByName(inPodIface)
	if err != nil {
		return
	}
	// ParentIndex is the host-side peer only for a veth
	if link.Type() != "veth" {
		return
	}
	hostIdx := link.Attrs().ParentIndex
	if hostIdx == 0 {
		return
	}

	addrs, err := h.AddrList(link, unix.AF_INET)
	if err != nil {
		return
	}
	for _, a := range addrs {
		if a.IP == nil || a.IP.IsLoopback() {
			continue
		}
		out[a.IP.String()] = hostIdx
	}
}
