// SPDX-FileCopyrightText: 2026 The latch Contributors
//
// SPDX-License-Identifier: Apache-2.0

package podveth

import (
	"errors"
	"reflect"
	"testing"
)

type fakeResolver struct {
	mappings map[string]int
	err      error
}

func (f fakeResolver) Resolve() (map[string]int, error) { return f.mappings, f.err }

type fakeAttacher struct {
	called  bool
	desired map[int]struct{}
}

func (f *fakeAttacher) Reconcile(desired map[int]struct{}) {
	f.called = true
	f.desired = desired
}

func TestEnforceOnce_GatesOnNamespaceAndPolicy(t *testing.T) {
	r := fakeResolver{mappings: map[string]int{
		"10.0.0.1": 11, // default ns, policy matches   → attach
		"10.0.0.2": 22, // kube-system, no policy        → skip
		"10.0.0.3": 33, // not a tracked pod (host netns) → skip
	}}

	nsFn := func(ip string) (string, map[string]string) {
		switch ip {
		case "10.0.0.1":
			return "default", map[string]string{"app": "web"}
		case "10.0.0.2":
			return "kube-system", nil
		default:
			return "", nil
		}
	}
	coveredFn := func(ns string, _ map[string]string) bool { return ns == "default" }

	att := &fakeAttacher{}
	enforceOnce(r, nsFn, coveredFn, att)

	if !att.called {
		t.Fatal("expected Reconcile to be called")
	}
	want := map[int]struct{}{11: {}}
	if !reflect.DeepEqual(att.desired, want) {
		t.Errorf("desired = %v, want %v", att.desired, want)
	}
}

func TestEnforceOnce_SkipsReconcileOnResolveError(t *testing.T) {
	r := fakeResolver{err: errors.New("resolve failed")}
	nsFn := func(string) (string, map[string]string) { return "default", nil }
	coveredFn := func(string, map[string]string) bool { return true }

	att := &fakeAttacher{}
	enforceOnce(r, nsFn, coveredFn, att)

	if att.called {
		t.Error("expected Reconcile NOT to be called when resolve fails (would detach everything)")
	}
}
