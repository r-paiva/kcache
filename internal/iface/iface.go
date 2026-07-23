// SPDX-FileCopyrightText: Copyright (c) 2026, the kcache developers
//
// SPDX-License-Identifier: Apache-2.0

package iface

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"syscall"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// narrow interface so Manager is testable without a real kernel.
type linkCloser interface {
	Close() error
}

// DefaultIfacePattern matches pod-facing veth interfaces by CNI naming convention:
//   Flannel/containerd:  veth + 8 hex chars  (e.g. veth1a2b3c4d)
//   Cilium veth mode:    lxc  + 12 hex chars (e.g. lxcaf76531335cb)
//   Calico:              cali + 10 hex chars (e.g. cali1a2b3c4d5e)
// Cilium's internal interfaces (cilium_net, cilium_host) are excluded — attaching
// TC programs there breaks Cilium's packet forwarding.
//
// NOTE: on nodes that run both k8s (Flannel/containerd) and Docker, the veth
// pattern also matches Docker container interfaces because Docker uses the same
// naming scheme. On Cilium clusters use the narrower "lxc[0-9a-f]{8,}" pattern
// via the --iface-pattern flag to restrict interception to k8s pods only.
const DefaultIfacePattern = `^(veth[0-9a-f]{7,}|lxc[0-9a-f]{8,}|cali[0-9a-f]{7,})$`

type Manager struct {
	ingress *ebpf.Program
	egress  *ebpf.Program
	podRe   *regexp.Regexp

	mu    sync.Mutex
	links map[int][]linkCloser // ifindex → open TCX links for that interface
}

// New creates a Manager that attaches TC BPF programs to interfaces whose names
// match pattern. pattern must be a valid Go regexp; wrap it in ^(...)$ to anchor
// it. Pass DefaultIfacePattern unless you need CNI-specific narrowing.
func New(ingress, egress *ebpf.Program, pattern string) *Manager {
	return &Manager{
		ingress: ingress,
		egress:  egress,
		podRe:   regexp.MustCompile(pattern),
		links:   make(map[int][]linkCloser),
	}
}

func (m *Manager) AttachExisting() error {
	links, err := netlink.LinkList()
	if err != nil {
		return fmt.Errorf("list links: %w", err)
	}
	for _, l := range links {
		if !m.isPodVeth(l) {
			continue
		}
		if err := m.attach(l); err != nil {
			slog.Warn("attach TC to existing veth", "iface", l.Attrs().Name, "err", err)
		}
	}
	return nil
}

func (m *Manager) Watch(ctx context.Context) {
	ch := make(chan netlink.LinkUpdate, 16)
	done := make(chan struct{})
	if err := netlink.LinkSubscribe(ch, done); err != nil {
		slog.Error("netlink link subscribe", "err", err)
		return
	}
	defer close(done)

	for {
		select {
		case <-ctx.Done():
			return
		case update, ok := <-ch:
			if !ok {
				return
			}
			switch update.Header.Type {
			case unix.RTM_NEWLINK:
				if !m.isPodVeth(update.Link) {
					continue
				}
				if err := m.attach(update.Link); err != nil {
					slog.Warn("attach TC to new veth", "iface", update.Link.Attrs().Name, "err", err)
				}
			case unix.RTM_DELLINK:
				m.detach(update.Link.Attrs().Index, update.Link.Attrs().Name)
			}
		}
	}
}

func (m *Manager) detach(ifindex int, name string) {
	// Kernel already removed TC programs when the interface was destroyed;
	// release the link FDs and free the ifindex for reuse by a future pod.
	m.mu.Lock()
	ls, ok := m.links[ifindex]
	delete(m.links, ifindex)
	m.mu.Unlock()
	if !ok {
		return
	}
	for _, l := range ls {
		_ = l.Close()
	}
	slog.Info("TC BPF detached", "iface", name)
}

func (m *Manager) isPodVeth(l netlink.Link) bool {
	return l.Type() == "veth" && m.podRe.MatchString(l.Attrs().Name)
}

func (m *Manager) attach(l netlink.Link) error {
	idx := l.Attrs().Index
	name := l.Attrs().Name

	m.mu.Lock()
	if _, already := m.links[idx]; already {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	var ls []linkCloser

	// TCX (kernel ≥6.6) puts kcache in the same chain as Cilium; fall back to cls_bpf.
	ing, egr, err := attachTCX(idx, m.ingress, m.egress)
	if err != nil {
		slog.Debug("TCX not available, falling back to cls_bpf", "iface", name, "err", err)
		if err2 := attachLegacy(idx, m.ingress, m.egress); err2 != nil {
			return fmt.Errorf("attach TC (tcx: %v, legacy: %w)", err, err2)
		}
	} else {
		ls = append(ls, ing, egr)
	}

	m.mu.Lock()
	m.links[idx] = ls
	m.mu.Unlock()

	slog.Info("TC BPF attached", "iface", name)
	return nil
}

// link.Head() is required — Cilium's TC programs return TC_ACT_REDIRECT which
// terminates the chain, so kcache must run before them.
func attachTCX(ifindex int, ingress, egress *ebpf.Program) (link.Link, link.Link, error) {
	ing, err := link.AttachTCX(link.TCXOptions{
		Interface: ifindex,
		Program:   ingress,
		Attach:    ebpf.AttachTCXIngress,
		Anchor:    link.Head(),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("tcx ingress: %w", err)
	}

	egr, err := link.AttachTCX(link.TCXOptions{
		Interface: ifindex,
		Program:   egress,
		Attach:    ebpf.AttachTCXEgress,
		Anchor:    link.Head(),
	})
	if err != nil {
		_ = ing.Close()
		return nil, nil, fmt.Errorf("tcx egress: %w", err)
	}

	return ing, egr, nil
}

func attachLegacy(ifindex int, ingress, egress *ebpf.Program) error {
	if err := ensureClsact(ifindex); err != nil {
		return fmt.Errorf("clsact qdisc: %w", err)
	}
	if err := replaceFilter(ifindex, ingress, netlink.HANDLE_MIN_INGRESS, "kcache/ingress"); err != nil {
		return fmt.Errorf("ingress filter: %w", err)
	}
	if err := replaceFilter(ifindex, egress, netlink.HANDLE_MIN_EGRESS, "kcache/egress"); err != nil {
		return fmt.Errorf("egress filter: %w", err)
	}
	return nil
}

func ensureClsact(ifindex int) error {
	qdisc := &netlink.GenericQdisc{
		QdiscAttrs: netlink.QdiscAttrs{
			LinkIndex: ifindex,
			Handle:    netlink.MakeHandle(0xffff, 0),
			Parent:    netlink.HANDLE_CLSACT,
		},
		QdiscType: "clsact",
	}
	err := netlink.QdiscAdd(qdisc)
	if err != nil && !errors.Is(err, syscall.EEXIST) {
		return err
	}
	return nil
}

func replaceFilter(ifindex int, prog *ebpf.Program, parent uint32, label string) error {
	filter := &netlink.BpfFilter{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: ifindex,
			Parent:    parent,
			Handle:    netlink.MakeHandle(0, 1),
			Protocol:  unix.ETH_P_ALL,
			Priority:  1,
		},
		Fd:           prog.FD(),
		Name:         label,
		DirectAction: true,
	}
	err := netlink.FilterReplace(filter)
	if err != nil {
		if addErr := netlink.FilterAdd(filter); addErr != nil && !errors.Is(addErr, syscall.EEXIST) {
			return addErr
		}
	}
	return nil
}
