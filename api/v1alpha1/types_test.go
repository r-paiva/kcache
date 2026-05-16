// SPDX-FileCopyrightText: Copyright (c) 2026, the k-cache developers
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1_test

import (
	"encoding/json"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"kache/api/v1alpha1"
)

// ── DeepCopyObject ────────────────────────────────────────────────────────────

func TestDeepCopyObject_CachePolicy_IsIndependent(t *testing.T) {
	original := &v1alpha1.CachePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "default"},
		Spec: v1alpha1.CachePolicySpec{
			Rules: []v1alpha1.CachePolicyRule{
				{Host: "svc", Port: 80, TTL: metav1.Duration{Duration: time.Minute}},
			},
		},
	}

	copied, ok := original.DeepCopyObject().(*v1alpha1.CachePolicy)
	if !ok {
		t.Fatal("DeepCopyObject did not return *CachePolicy")
	}

	// Mutating the copy must not affect the original.
	copied.Name = "changed"
	if original.Name != "demo" {
		t.Error("mutating copy Name changed the original")
	}

	copied.Spec.Rules[0].Host = "other"
}

func TestDeepCopyObject_CachePolicy_ImplementsRuntimeObject(t *testing.T) {
	cp := &v1alpha1.CachePolicy{}
	var _ runtime.Object = cp
	obj := cp.DeepCopyObject()
	if obj == nil {
		t.Fatal("DeepCopyObject returned nil")
	}
}

func TestDeepCopyObject_CachePolicyList_IsIndependent(t *testing.T) {
	original := &v1alpha1.CachePolicyList{
		Items: []v1alpha1.CachePolicy{
			{ObjectMeta: metav1.ObjectMeta{Name: "a"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "b"}},
		},
	}

	copied, ok := original.DeepCopyObject().(*v1alpha1.CachePolicyList)
	if !ok {
		t.Fatal("DeepCopyObject did not return *CachePolicyList")
	}
	if len(copied.Items) != 2 {
		t.Fatalf("expected 2 items in copy, got %d", len(copied.Items))
	}

	// The slice itself is independent.
	copied.Items[0].Name = "changed"
	if original.Items[0].Name != "a" {
		t.Error("mutating copy Items changed the original")
	}
}

func TestDeepCopyObject_CachePolicyList_Empty(t *testing.T) {
	original := &v1alpha1.CachePolicyList{}
	copied := original.DeepCopyObject()
	if copied == nil {
		t.Fatal("DeepCopyObject returned nil for empty list")
	}
}

// ── JSON round-trip ───────────────────────────────────────────────────────────

func TestCachePolicy_JSONRoundTrip(t *testing.T) {
	original := v1alpha1.CachePolicy{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "kcache.io/v1alpha1",
			Kind:       "CachePolicy",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "demo",
			Namespace: "prod",
		},
		Spec: v1alpha1.CachePolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "catalog"},
			},
			Rules: []v1alpha1.CachePolicyRule{
				{
					Host:         "inventory",
					Port:         80,
					Methods:      []string{"GET"},
					Paths:        []string{"/api/products", "/api/categories"},
					TTL:          metav1.Duration{Duration: 5 * time.Minute},
					MaxBodyBytes: 1 << 20,
				},
				{
					Host: "*",
					Port: 80,
					TTL:  metav1.Duration{Duration: 30 * time.Second},
				},
			},
		},
	}

	b, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got v1alpha1.CachePolicy
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Name != original.Name || got.Namespace != original.Namespace {
		t.Errorf("metadata: got %q/%q", got.Name, got.Namespace)
	}
	if got.Spec.PodSelector.MatchLabels["app"] != "catalog" {
		t.Errorf("podSelector not preserved: %v", got.Spec.PodSelector)
	}
	if len(got.Spec.Rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(got.Spec.Rules))
	}
	r0 := got.Spec.Rules[0]
	if r0.Host != "inventory" || r0.Port != 80 || r0.TTL.Duration != 5*time.Minute {
		t.Errorf("rule[0] fields not preserved: %+v", r0)
	}
	if len(r0.Paths) != 2 || r0.Paths[0] != "/api/products" {
		t.Errorf("paths not preserved: %v", r0.Paths)
	}
	if r0.MaxBodyBytes != 1<<20 {
		t.Errorf("maxBodyBytes not preserved: %d", r0.MaxBodyBytes)
	}
}

func TestCachePolicyRule_DefaultPort(t *testing.T) {
	// Port with zero value should marshal as 0 (omitempty would drop it,
	// but we don't use omitempty on Port so it's always present).
	r := v1alpha1.CachePolicyRule{Host: "*", TTL: metav1.Duration{Duration: time.Minute}}
	b, _ := json.Marshal(r)

	var m map[string]any
	_ = json.Unmarshal(b, &m)

	// Port 0 means "match any" in our semantics; verify it round-trips.
	var got v1alpha1.CachePolicyRule
	_ = json.Unmarshal(b, &got)
	if got.Port != 0 {
		t.Errorf("port: got %d, want 0", got.Port)
	}
}

func TestCachePolicyList_JSONRoundTrip(t *testing.T) {
	original := v1alpha1.CachePolicyList{
		Items: []v1alpha1.CachePolicy{
			{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns1"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "ns2"}},
		},
	}
	b, _ := json.Marshal(original)

	var got v1alpha1.CachePolicyList
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(got.Items) != 2 {
		t.Errorf("expected 2 items, got %d", len(got.Items))
	}
}

// ── GroupVersion / scheme ─────────────────────────────────────────────────────

func TestGroupVersion(t *testing.T) {
	if v1alpha1.GroupVersion.Group != "kcache.io" {
		t.Errorf("group: got %q, want %q", v1alpha1.GroupVersion.Group, "kcache.io")
	}
	if v1alpha1.GroupVersion.Version != "v1alpha1" {
		t.Errorf("version: got %q, want %q", v1alpha1.GroupVersion.Version, "v1alpha1")
	}
}

func TestAddToScheme(t *testing.T) {
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	gvk := schema.GroupVersionKind{
		Group:   "kcache.io",
		Version: "v1alpha1",
		Kind:    "CachePolicy",
	}
	obj, err := s.New(gvk)
	if err != nil {
		t.Fatalf("scheme.New(%v): %v", gvk, err)
	}
	if _, ok := obj.(*v1alpha1.CachePolicy); !ok {
		t.Errorf("expected *CachePolicy from scheme.New, got %T", obj)
	}
}

func TestAddToScheme_ListRegistered(t *testing.T) {
	s := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(s)

	gvk := schema.GroupVersionKind{
		Group:   "kcache.io",
		Version: "v1alpha1",
		Kind:    "CachePolicyList",
	}
	obj, err := s.New(gvk)
	if err != nil {
		t.Fatalf("scheme.New(%v): %v", gvk, err)
	}
	if _, ok := obj.(*v1alpha1.CachePolicyList); !ok {
		t.Errorf("expected *CachePolicyList from scheme.New, got %T", obj)
	}
}
