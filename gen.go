// SPDX-FileCopyrightText: Copyright (c) 2026, the kcache developers
//
// SPDX-License-Identifier: Apache-2.0

package main

//go:generate go tool bpf2go -cflags "-Wall -Wextra -Werror" -tags linux kcache ./bpf/kcache.c
