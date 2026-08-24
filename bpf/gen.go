// SPDX-FileCopyrightText: 2026 The latch Contributors
//
// SPDX-License-Identifier: Apache-2.0

package bpf

//go:generate go tool bpf2go -cflags "-Wall -Wextra -Werror" -tags linux latch ./latch.c
