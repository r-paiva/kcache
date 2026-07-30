// SPDX-FileCopyrightText: Copyright (c) 2026, the latch developers
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import "k8s.io/apimachinery/pkg/runtime"

func (in *CachePolicy) DeepCopyObject() runtime.Object {
	out := *in
	return &out
}

func (in *CachePolicyList) DeepCopyObject() runtime.Object {
	out := *in
	out.Items = make([]CachePolicy, len(in.Items))
	copy(out.Items, in.Items)
	return &out
}
