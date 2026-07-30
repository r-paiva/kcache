// SPDX-FileCopyrightText: Copyright (c) 2026, the latch developers
//
// SPDX-License-Identifier: Apache-2.0

package tlsmitm

import (
	"bufio"
	"encoding/binary"
)

// PeekSNI extracts the TLS SNI hostname from a ClientHello without consuming
// bytes from r. Returns an empty string (no error) if SNI is absent or the
// record cannot be parsed.
func PeekSNI(r *bufio.Reader) (string, error) {
	hdr, err := r.Peek(5)
	if err != nil {
		return "", err
	}
	if hdr[0] != 0x16 { // TLS record type: handshake
		return "", nil
	}

	recLen := int(binary.BigEndian.Uint16(hdr[3:5]))
	if recLen > 16384 { // TLS max record size
		return "", nil
	}

	data, err := r.Peek(5 + recLen)
	if err != nil || len(data) < 9 {
		return "", nil
	}
	if data[5] != 0x01 { // handshake type: ClientHello
		return "", nil
	}

	// Skip: TLS record header (5), handshake header (4), client_version (2), random (32).
	pos := 5 + 4 + 2 + 32

	if pos+1 > len(data) {
		return "", nil
	}
	sidLen := int(data[pos])
	pos += 1 + sidLen

	if pos+2 > len(data) {
		return "", nil
	}
	csLen := int(binary.BigEndian.Uint16(data[pos : pos+2]))
	pos += 2 + csLen

	if pos+1 > len(data) {
		return "", nil
	}
	compLen := int(data[pos])
	pos += 1 + compLen

	if pos+2 > len(data) {
		return "", nil
	}
	extTotal := int(binary.BigEndian.Uint16(data[pos : pos+2]))
	pos += 2

	extEnd := pos + extTotal
	if extEnd > len(data) {
		extEnd = len(data)
	}

	for pos+4 <= extEnd {
		extType := binary.BigEndian.Uint16(data[pos : pos+2])
		extLen := int(binary.BigEndian.Uint16(data[pos+2 : pos+4]))
		pos += 4

		if pos+extLen > extEnd {
			break
		}

		if extType == 0x0000 && extLen >= 5 { // SNI extension (type 0)
			// server_name_list: uint16 length, then entries of:
			//   name_type (1 byte) + name_length (uint16) + name bytes
			p := pos + 2
			if p+3 > pos+extLen {
				break
			}
			if data[p] != 0x00 { // name_type must be host_name (0)
				break
			}
			nameLen := int(binary.BigEndian.Uint16(data[p+1 : p+3]))
			if p+3+nameLen > pos+extLen {
				break
			}
			return string(data[p+3 : p+3+nameLen]), nil
		}

		pos += extLen
	}

	return "", nil
}
