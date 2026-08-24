// SPDX-FileCopyrightText: 2026 The latch Contributors
//
// SPDX-License-Identifier: Apache-2.0

package bpf

import (
	"encoding/binary"
	"log/slog"
	"net"
	"os"
	"strconv"

	"codeberg.org/latch/latch/internal/constants"
)

// alias because bpf2go generates latchObjects lowercase
type LatchObjects = latchObjects

func Load(proxyAddr string) LatchObjects {
	var proxyTgt struct {
		IP   uint32
		Port uint16
		Pad  uint16
	}

	var ebpfObjs LatchObjects
	if err := loadLatchObjects(&ebpfObjs, nil); err != nil {
		slog.Error("load BPF objects", "err", err)
		os.Exit(constants.ExitEbpfLoadError)
	}
	slog.Debug("bpf objects loaded.")

	// Populate the proxy redirect target so the TC ingress program knows
	// where to send intercepted connections.
	// NODE_IP is injected via the downward API in Kubernetes; falls back to
	// 127.0.0.1 for local testing.
	redirectIP := net.ParseIP(os.Getenv("NODE_IP")).To4()
	if redirectIP == nil {
		redirectIP = net.IPv4(127, 0, 0, 1).To4()
	}
	slog.Debug("redirect IP for bpf", "ip", redirectIP)
	_, portStr, err := net.SplitHostPort(proxyAddr)
	if err != nil {
		slog.Error("parse proxy-addr", "err", err)
		os.Exit(constants.ExitEbpfLoadError)
	}

	redirectPort, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		slog.Error("parse proxy port", "err", err)
		os.Exit(constants.ExitEbpfLoadError)
	}

	proxyTgt.IP = binary.BigEndian.Uint32(redirectIP)
	proxyTgt.Port = uint16(redirectPort)
	tgtKey := uint32(0)
	if err := ebpfObjs.ProxyTgtMap.Put(&tgtKey, &proxyTgt); err != nil {
		slog.Error("set BPF proxy target", "err", err)
		os.Exit(constants.ExitEbpfLoadError)
	}

	slog.Info("BPF proxy target configured", "ip", redirectIP.String(), "port", redirectPort)
	return ebpfObjs
}
