// SPDX-FileCopyrightText: Copyright (c) 2026, the latch developers
//
// SPDX-License-Identifier: Apache-2.0

package main

//go:generate go tool bpf2go -cflags "-Wall -Wextra -Werror" -tags linux latch ./bpf/latch.c
