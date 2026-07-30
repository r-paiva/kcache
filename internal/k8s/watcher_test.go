// SPDX-FileCopyrightText: Copyright (c) 2026, the latch developers
//
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	clientcache "k8s.io/client-go/tools/cache"

	v1alpha1 "codeberg.org/latch/latch/api/v1alpha1"
	intpolicy "codeberg.org/latch/latch/internal/policy"
)

type fakeUnstructured struct{ content map[string]any }

func (f *fakeUnstructured) UnstructuredContent() map[string]any                    { return f.content }
func (f *fakeUnstructured) SetUnstructuredContent(c map[string]any)                { f.content = c }
func (f *fakeUnstructured) IsList() bool                                           { return false }
func (f *fakeUnstructured) EachListItem(func(runtime.Object) error) error          { return nil }
func (f *fakeUnstructured) EachListItemWithAlloc(func(runtime.Object) error) error { return nil }
func (f *fakeUnstructured) NewEmptyInstance() runtime.Unstructured                 { return &fakeUnstructured{} }
func (f *fakeUnstructured) GetObjectKind() schema.ObjectKind                       { return schema.EmptyObjectKind }
func (f *fakeUnstructured) DeepCopyObject() runtime.Object {
	c := make(map[string]any, len(f.content))
	for k, v := range f.content {
		c[k] = v
	}
	return &fakeUnstructured{content: c}
}

func makeWatcher(onChange func(*intpolicy.Policy)) *Watcher {
	if onChange == nil {
		onChange = func(*intpolicy.Policy) {}
	}
	return &Watcher{
		podMeta:  make(map[string]PodMeta),
		polMap:   make(map[string][]v1alpha1.CachePolicy),
		onChange: onChange,
	}
}

func makePod(ip, ns string, lbls map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-" + ip,
			Namespace: ns,
			Labels:    lbls,
		},
		Status: corev1.PodStatus{PodIP: ip},
	}
}

func makePolicy(name, ns string, sel *metav1.LabelSelector, ttl time.Duration) v1alpha1.CachePolicy {
	cp := v1alpha1.CachePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1alpha1.CachePolicySpec{
			Rules: []v1alpha1.CachePolicyRule{
				{Host: "*", Port: 80, TTL: metav1.Duration{Duration: ttl}},
			},
		},
	}
	if sel != nil {
		cp.Spec.PodSelector = *sel
	}
	return cp
}

func policyToUnstructured(cp v1alpha1.CachePolicy) map[string]any {
	b, _ := json.Marshal(cp)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func TestOnPodAdd_StoresMeta(t *testing.T) {
	w := makeWatcher(nil)
	w.onPodAdd(makePod("10.0.0.1", "prod", map[string]string{"app": "api"}))

	ns, lbls := w.NamespaceLookup("10.0.0.1")
	if ns != "prod" {
		t.Errorf("namespace: got %q, want %q", ns, "prod")
	}
	if lbls["app"] != "api" {
		t.Errorf("label app: got %q, want %q", lbls["app"], "api")
	}
}

func TestOnPodAdd_IgnoresPodWithNoIP(t *testing.T) {
	w := makeWatcher(nil)
	pod := makePod("", "prod", nil)
	w.onPodAdd(pod)
	if len(w.podMeta) != 0 {
		t.Error("expected podMeta to remain empty for pod with no IP")
	}
}

func TestOnPodAdd_IgnoresNonPod(t *testing.T) {
	w := makeWatcher(nil)
	w.onPodAdd("not-a-pod")
	if len(w.podMeta) != 0 {
		t.Error("expected podMeta to remain empty for non-pod object")
	}
}

func TestOnPodDelete_RemovesEntry(t *testing.T) {
	w := makeWatcher(nil)
	w.onPodAdd(makePod("10.0.0.2", "prod", nil))
	w.onPodDelete(makePod("10.0.0.2", "prod", nil))

	if _, lbls := w.NamespaceLookup("10.0.0.2"); lbls != nil {
		t.Error("expected pod to be removed after delete")
	}
}

func TestOnPodDelete_Tombstone(t *testing.T) {
	w := makeWatcher(nil)
	pod := makePod("10.0.0.3", "prod", nil)
	w.onPodAdd(pod)

	tombstone := clientcache.DeletedFinalStateUnknown{Key: "prod/pod-10.0.0.3", Obj: pod}
	w.onPodDelete(tombstone)

	if _, lbls := w.NamespaceLookup("10.0.0.3"); lbls != nil {
		t.Error("expected pod to be removed via tombstone delete")
	}
}

func TestOnPodDelete_UnknownIPIsNoop(t *testing.T) {
	w := makeWatcher(nil)
	w.onPodDelete(makePod("10.0.0.99", "prod", nil)) // never added
	// Should not panic.
}

func TestNamespaceLookup_Found(t *testing.T) {
	w := makeWatcher(nil)
	w.onPodAdd(makePod("192.168.1.5", "staging", map[string]string{"tier": "frontend"}))

	ns, lbls := w.NamespaceLookup("192.168.1.5")
	if ns != "staging" || lbls["tier"] != "frontend" {
		t.Errorf("unexpected result: ns=%q labels=%v", ns, lbls)
	}
}

func TestNamespaceLookup_NotFound(t *testing.T) {
	w := makeWatcher(nil)
	ns, lbls := w.NamespaceLookup("1.2.3.4")
	if ns != "" || lbls != nil {
		t.Errorf("expected empty result for unknown IP, got ns=%q", ns)
	}
}

func TestHandlePolicyEvent_Added(t *testing.T) {
	w := makeWatcher(nil)
	cp := makePolicy("cache-all", "default", nil, time.Minute)
	ev := watch.Event{
		Type:   watch.Added,
		Object: &fakeUnstructured{content: policyToUnstructured(cp)},
	}
	w.handlePolicyEvent(ev)

	if len(w.polMap["default"]) != 1 {
		t.Fatalf("expected 1 policy in default namespace, got %d", len(w.polMap["default"]))
	}
	if w.polMap["default"][0].Name != "cache-all" {
		t.Errorf("unexpected policy name: %q", w.polMap["default"][0].Name)
	}
}

func TestHandlePolicyEvent_Modified(t *testing.T) {
	w := makeWatcher(nil)
	// Add initial policy with 1 minute TTL.
	cp := makePolicy("cache-all", "default", nil, time.Minute)
	w.handlePolicyEvent(watch.Event{
		Type:   watch.Added,
		Object: &fakeUnstructured{content: policyToUnstructured(cp)},
	})

	// Modify to 5 minutes TTL.
	updated := makePolicy("cache-all", "default", nil, 5*time.Minute)
	w.handlePolicyEvent(watch.Event{
		Type:   watch.Modified,
		Object: &fakeUnstructured{content: policyToUnstructured(updated)},
	})

	if len(w.polMap["default"]) != 1 {
		t.Fatalf("expected exactly 1 policy after modify, got %d", len(w.polMap["default"]))
	}
	got := w.polMap["default"][0].Spec.Rules[0].TTL.Duration
	if got != 5*time.Minute {
		t.Errorf("TTL after modify: got %v, want 5m", got)
	}
}

func TestHandlePolicyEvent_Deleted(t *testing.T) {
	w := makeWatcher(nil)
	cp := makePolicy("cache-all", "default", nil, time.Minute)
	w.handlePolicyEvent(watch.Event{
		Type:   watch.Added,
		Object: &fakeUnstructured{content: policyToUnstructured(cp)},
	})
	w.handlePolicyEvent(watch.Event{
		Type:   watch.Deleted,
		Object: &fakeUnstructured{content: policyToUnstructured(cp)},
	})

	if len(w.polMap["default"]) != 0 {
		t.Errorf("expected empty policy list after delete, got %d", len(w.polMap["default"]))
	}
}

func TestHandlePolicyEvent_DeleteNonExistentIsNoop(t *testing.T) {
	w := makeWatcher(nil)
	cp := makePolicy("ghost", "default", nil, time.Minute)
	// Should not panic.
	w.handlePolicyEvent(watch.Event{
		Type:   watch.Deleted,
		Object: &fakeUnstructured{content: policyToUnstructured(cp)},
	})
}

func TestHandlePolicyEvent_MultipleNamespaces(t *testing.T) {
	w := makeWatcher(nil)
	w.handlePolicyEvent(watch.Event{
		Type:   watch.Added,
		Object: &fakeUnstructured{content: policyToUnstructured(makePolicy("pol", "ns-a", nil, time.Minute))},
	})
	w.handlePolicyEvent(watch.Event{
		Type:   watch.Added,
		Object: &fakeUnstructured{content: policyToUnstructured(makePolicy("pol", "ns-b", nil, 30*time.Second))},
	})

	if len(w.polMap["ns-a"]) != 1 || len(w.polMap["ns-b"]) != 1 {
		t.Error("expected one policy per namespace")
	}
}

func TestHandlePolicyEvent_InvalidObject(t *testing.T) {
	w := makeWatcher(nil)
	w.handlePolicyEvent(watch.Event{
		Type:   watch.Added,
		Object: nil,
	})
	if len(w.polMap) != 0 {
		t.Error("expected no change when event object is nil/invalid")
	}
}

func TestRebuild_EmptyMap(t *testing.T) {
	var got *intpolicy.Policy
	w := makeWatcher(func(p *intpolicy.Policy) { got = p })
	w.rebuild()

	if got == nil {
		t.Fatal("onChange should be called even with empty polMap")
	}
	// Empty policy — no rules match anything.
	if r := got.Match("", nil, "svc", 80, "GET", "/"); r != nil {
		t.Error("expected empty policy to match nothing")
	}
}

func TestRebuild_SinglePolicy(t *testing.T) {
	var got *intpolicy.Policy
	w := makeWatcher(func(p *intpolicy.Policy) { got = p })
	w.polMap["default"] = []v1alpha1.CachePolicy{
		makePolicy("p1", "default", nil, 2*time.Minute),
	}
	w.rebuild()

	r := got.Match("default", nil, "any-host", 80, "GET", "/")
	if r == nil {
		t.Fatal("expected a matching rule")
	}
	if r.TTL != 2*time.Minute {
		t.Errorf("TTL: got %v, want 2m", r.TTL)
	}
}

func TestRebuild_MultipleNamespaces(t *testing.T) {
	var got *intpolicy.Policy
	w := makeWatcher(func(p *intpolicy.Policy) { got = p })
	w.polMap["ns-a"] = []v1alpha1.CachePolicy{makePolicy("p", "ns-a", nil, time.Minute)}
	w.polMap["ns-b"] = []v1alpha1.CachePolicy{makePolicy("p", "ns-b", nil, 30*time.Second)}
	w.rebuild()

	rA := got.Match("ns-a", nil, "*", 80, "GET", "/")
	rB := got.Match("ns-b", nil, "*", 80, "GET", "/")
	if rA == nil || rB == nil {
		t.Fatal("expected rules for both namespaces")
	}
	if rA.TTL != time.Minute || rB.TTL != 30*time.Second {
		t.Errorf("TTLs: ns-a=%v ns-b=%v", rA.TTL, rB.TTL)
	}
}

func TestRebuild_MoreSpecificSelectorTakesPriority(t *testing.T) {
	var got *intpolicy.Policy
	w := makeWatcher(func(p *intpolicy.Policy) { got = p })

	broad := makePolicy("broad", "default", nil, time.Minute)
	narrow := makePolicy("narrow", "default", &metav1.LabelSelector{
		MatchLabels: map[string]string{"app": "api"},
	}, 5*time.Minute)

	w.polMap["default"] = []v1alpha1.CachePolicy{broad, narrow}
	w.rebuild()

	podLabels := map[string]string{"app": "api"}
	r := got.Match("default", podLabels, "*", 80, "GET", "/")
	if r == nil {
		t.Fatal("expected a rule to match")
	}
	if r.TTL != 5*time.Minute {
		t.Errorf("narrow policy (TTL=5m) should win; got TTL=%v", r.TTL)
	}
}

func TestRebuild_InvalidSelectorSkipped(t *testing.T) {
	var got *intpolicy.Policy
	w := makeWatcher(func(p *intpolicy.Policy) { got = p })

	bad := makePolicy("bad", "default", &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "app", Operator: "InvalidOp", Values: []string{"x"}},
		},
	}, time.Minute)
	w.polMap["default"] = []v1alpha1.CachePolicy{bad}
	w.rebuild()

	if r := got.Match("default", nil, "*", 80, "GET", "/"); r != nil {
		t.Error("policy with invalid selector should be skipped")
	}
}

func TestPolicyForPod_NoMatchingNamespace(t *testing.T) {
	w := makeWatcher(nil)
	w.polMap["other"] = []v1alpha1.CachePolicy{makePolicy("p", "other", nil, time.Minute)}

	if rules := w.PolicyForPod("default", nil); rules != nil {
		t.Errorf("expected nil for unknown namespace, got %d rules", len(rules))
	}
}

func TestPolicyForPod_EmptySelectorMatchesAll(t *testing.T) {
	w := makeWatcher(nil)
	w.polMap["default"] = []v1alpha1.CachePolicy{makePolicy("p", "default", nil, time.Minute)}

	rules := w.PolicyForPod("default", map[string]string{"app": "anything"})
	if len(rules) == 0 {
		t.Error("empty selector should match all pods")
	}
}

func TestPolicyForPod_LabelMatchReturnsRules(t *testing.T) {
	w := makeWatcher(nil)
	cp := makePolicy("p", "default", &metav1.LabelSelector{
		MatchLabels: map[string]string{"app": "api"},
	}, time.Minute)
	w.polMap["default"] = []v1alpha1.CachePolicy{cp}

	rules := w.PolicyForPod("default", map[string]string{"app": "api"})
	if len(rules) == 0 {
		t.Error("expected rules for matching pod labels")
	}
}

func TestPolicyForPod_LabelMismatchReturnsNil(t *testing.T) {
	w := makeWatcher(nil)
	cp := makePolicy("p", "default", &metav1.LabelSelector{
		MatchLabels: map[string]string{"app": "api"},
	}, time.Minute)
	w.polMap["default"] = []v1alpha1.CachePolicy{cp}

	rules := w.PolicyForPod("default", map[string]string{"app": "frontend"})
	if rules != nil {
		t.Error("expected nil when pod labels don't match the selector")
	}
}

func TestPolicyForPod_MostSpecificWins(t *testing.T) {
	w := makeWatcher(nil)
	broad := makePolicy("broad", "default", nil, time.Minute)
	narrow := makePolicy("narrow", "default", &metav1.LabelSelector{
		MatchLabels: map[string]string{"app": "api"},
	}, 5*time.Minute)
	w.polMap["default"] = []v1alpha1.CachePolicy{broad, narrow}

	rules := w.PolicyForPod("default", map[string]string{"app": "api"})
	if len(rules) == 0 {
		t.Fatal("expected rules")
	}
	if rules[0].TTL != 5*time.Minute {
		t.Errorf("narrow policy should win; got TTL=%v", rules[0].TTL)
	}
}

func TestPolicyForPod_TieBreakByName(t *testing.T) {
	w := makeWatcher(nil)
	w.polMap["default"] = []v1alpha1.CachePolicy{
		makePolicy("zzz", "default", nil, time.Hour),
		makePolicy("aaa", "default", nil, 30*time.Second),
	}

	rules := w.PolicyForPod("default", nil)
	if len(rules) == 0 {
		t.Fatal("expected rules")
	}
	if rules[0].TTL != 30*time.Second {
		t.Errorf("alphabetically first name (aaa, TTL=30s) should win; got %v", rules[0].TTL)
	}
}

func TestUnstructuredToPolicy_RoundTrip(t *testing.T) {
	cp := v1alpha1.CachePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "default"},
		Spec: v1alpha1.CachePolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "api"},
			},
			Rules: []v1alpha1.CachePolicyRule{
				{Host: "svc", Port: 80, Methods: []string{"GET"}, Paths: []string{"/api/"}, TTL: metav1.Duration{Duration: time.Minute}},
			},
		},
	}

	got, err := unstructuredToPolicy(policyToUnstructured(cp))
	if err != nil {
		t.Fatalf("unstructuredToPolicy: %v", err)
	}
	if got.Name != "demo" || got.Namespace != "default" {
		t.Errorf("name/namespace: got %q/%q", got.Name, got.Namespace)
	}
	if len(got.Spec.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(got.Spec.Rules))
	}
	r := got.Spec.Rules[0]
	if r.Host != "svc" || r.Port != 80 || r.TTL.Duration != time.Minute {
		t.Errorf("rule fields not preserved: %+v", r)
	}
	if len(r.Methods) != 1 || r.Methods[0] != "GET" {
		t.Errorf("methods not preserved: %v", r.Methods)
	}
}

func TestUnstructuredToPolicy_InvalidJSON(t *testing.T) {
	// nil map produces an empty CachePolicy, not an error
	cp, err := unstructuredToPolicy(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cp.Name != "" {
		t.Errorf("expected empty CachePolicy from nil map")
	}
}

func TestSelectorLen_Nil(t *testing.T) {
	if n := selectorLen(nil); n != 0 {
		t.Errorf("nil selector: want 0, got %d", n)
	}
}

func TestSelectorLen_Empty(t *testing.T) {
	sel, _ := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{})
	if n := selectorLen(sel); n != 0 {
		t.Errorf("empty selector: want 0, got %d", n)
	}
}

func TestSelectorLen_WithRequirements(t *testing.T) {
	sel, _ := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
		MatchLabels: map[string]string{"app": "api", "env": "prod"},
	})
	if n := selectorLen(sel); n != 2 {
		t.Errorf("2-label selector: want 2, got %d", n)
	}
}
