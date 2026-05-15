// Package k8s watches CachePolicy CRDs and pod events to provide dynamic,
// namespace- and label-scoped cache policy to the kcache proxy.
package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	v1alpha1 "kache/api/v1alpha1"
	intpolicy "kache/internal/policy"
)

var cachePolicyGVR = schema.GroupVersionResource{
	Group:    "kcache.io",
	Version:  "v1alpha1",
	Resource: "cachepolicies",
}

// PodMeta holds the namespace and labels for a pod, keyed by pod IP.
type PodMeta struct {
	Namespace string
	Labels    map[string]string
}

// Watcher maintains a live view of CachePolicies and pod IPs.
// Call Run() to start the informers; it blocks until ctx is cancelled.
type Watcher struct {
	client    kubernetes.Interface
	dynClient dynamic.Interface

	mu      sync.RWMutex
	podMeta map[string]PodMeta  // podIP → PodMeta
	polMap  map[string][]v1alpha1.CachePolicy // namespace → policies

	onChange func(*intpolicy.Policy)
}

// New creates a Watcher. onChange is called whenever the effective policy
// changes; the caller should replace the proxy's active policy.
func New(onChange func(*intpolicy.Policy)) (*Watcher, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, fmt.Errorf("k8s config: %w", err)
	}
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("k8s client: %w", err)
	}
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("k8s dynamic client: %w", err)
	}
	return &Watcher{
		client:    kc,
		dynClient: dc,
		podMeta:   make(map[string]PodMeta),
		polMap:    make(map[string][]v1alpha1.CachePolicy),
		onChange:  onChange,
	}, nil
}

// loadConfig tries in-cluster config first, falls back to kubeconfig.
func loadConfig() (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, nil).ClientConfig()
}

// Run starts the pod and CachePolicy informers and blocks until ctx is done.
func (w *Watcher) Run(ctx context.Context) {
	factory := informers.NewSharedInformerFactory(w.client, 5*time.Minute)

	podInformer := factory.Core().V1().Pods().Informer()
	podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { w.onPodAdd(obj) },
		UpdateFunc: func(_, obj any) { w.onPodAdd(obj) },
		DeleteFunc: func(obj any) { w.onPodDelete(obj) },
	})

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())
	slog.Info("k8s pod informer synced")

	// CachePolicy is a CRD so we use the dynamic client with a manual watch loop.
	go w.watchCachePolicies(ctx)

	<-ctx.Done()
}

// NamespaceLookup returns the namespace and labels for the given pod IP.
// Returns empty strings/nil if the pod is not known.
func (w *Watcher) NamespaceLookup(podIP string) (namespace string, podLabels map[string]string) {
	w.mu.RLock()
	m, ok := w.podMeta[podIP]
	w.mu.RUnlock()
	if !ok {
		return "", nil
	}
	return m.Namespace, m.Labels
}

// ── pod handlers ─────────────────────────────────────────────────────────────

func (w *Watcher) onPodAdd(obj any) {
	pod, ok := obj.(*corev1.Pod)
	if !ok || pod.Status.PodIP == "" {
		return
	}
	w.mu.Lock()
	w.podMeta[pod.Status.PodIP] = PodMeta{
		Namespace: pod.Namespace,
		Labels:    pod.Labels,
	}
	w.mu.Unlock()
}

func (w *Watcher) onPodDelete(obj any) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		// Tombstone object from the informer cache.
		if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
			pod, ok = d.Obj.(*corev1.Pod)
			if !ok {
				return
			}
		} else {
			return
		}
	}
	w.mu.Lock()
	delete(w.podMeta, pod.Status.PodIP)
	w.mu.Unlock()
}

// ── CachePolicy watch ─────────────────────────────────────────────────────────

func (w *Watcher) watchCachePolicies(ctx context.Context) {
	for {
		if err := w.runCachePolicyWatch(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("CachePolicy watch error, retrying", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}
}

func (w *Watcher) runCachePolicyWatch(ctx context.Context) error {
	// Do an initial list to populate the map before watching.
	list, err := w.dynClient.Resource(cachePolicyGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list CachePolicies: %w", err)
	}

	newMap := make(map[string][]v1alpha1.CachePolicy)
	for _, item := range list.Items {
		cp, err := unstructuredToPolicy(item.Object)
		if err != nil {
			slog.Warn("decode CachePolicy", "name", item.GetName(), "err", err)
			continue
		}
		newMap[cp.Namespace] = append(newMap[cp.Namespace], cp)
	}
	w.mu.Lock()
	w.polMap = newMap
	w.mu.Unlock()
	w.rebuild()
	slog.Info("CachePolicy initial sync complete", "count", len(list.Items))

	watcher, err := w.dynClient.Resource(cachePolicyGVR).Namespace("").Watch(ctx, metav1.ListOptions{
		ResourceVersion: list.GetResourceVersion(),
	})
	if err != nil {
		return fmt.Errorf("watch CachePolicies: %w", err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-watcher.ResultChan():
			if !ok {
				return fmt.Errorf("watch channel closed")
			}
			w.handlePolicyEvent(ev)
		}
	}
}

func (w *Watcher) handlePolicyEvent(ev watch.Event) {
	u, ok := ev.Object.(runtime.Unstructured)
	if !ok {
		return
	}
	cp, err := unstructuredToPolicy(u.UnstructuredContent())
	if err != nil {
		slog.Warn("decode CachePolicy event", "err", err)
		return
	}

	w.mu.Lock()
	switch ev.Type {
	case watch.Added, watch.Modified:
		policies := w.polMap[cp.Namespace]
		replaced := false
		for i, p := range policies {
			if p.Name == cp.Name {
				policies[i] = cp
				replaced = true
				break
			}
		}
		if !replaced {
			policies = append(policies, cp)
		}
		w.polMap[cp.Namespace] = policies
		slog.Info("CachePolicy updated", "namespace", cp.Namespace, "name", cp.Name,
			"rules", len(cp.Spec.Rules))
	case watch.Deleted:
		policies := w.polMap[cp.Namespace]
		for i, p := range policies {
			if p.Name == cp.Name {
				w.polMap[cp.Namespace] = append(policies[:i], policies[i+1:]...)
				break
			}
		}
		slog.Info("CachePolicy deleted", "namespace", cp.Namespace, "name", cp.Name)
	}
	w.mu.Unlock()
	w.rebuild()
}

// ── policy rebuild ────────────────────────────────────────────────────────────

// rebuild converts the current CachePolicy map into a policy.Policy and
// calls onChange. Called whenever the map changes.
func (w *Watcher) rebuild() {
	w.mu.RLock()
	snapshot := make(map[string][]v1alpha1.CachePolicy, len(w.polMap))
	for ns, cps := range w.polMap {
		snapshot[ns] = append([]v1alpha1.CachePolicy{}, cps...)
	}
	w.mu.RUnlock()

	var rules []intpolicy.Rule
	for ns, cps := range snapshot {
		for _, cp := range cps {
			sel, err := metav1.LabelSelectorAsSelector(&cp.Spec.PodSelector)
			if err != nil {
				slog.Warn("invalid podSelector", "namespace", ns, "name", cp.Name, "err", err)
				continue
			}
			for _, r := range cp.Spec.Rules {
				rules = append(rules, intpolicy.Rule{
					Namespace:    ns,
					PodSelector:  sel,
					Host:         r.Host,
					Port:         r.Port,
					Methods:      r.Methods,
					Paths:        r.Paths,
					TTL:          r.TTL.Duration,
					MaxBodyBytes: r.MaxBodyBytes,
					VaryHeaders:  r.VaryHeaders,
					CacheBody:    r.CacheBody,
				})
			}
		}
	}

	// More specific selectors (more requirements) take priority.
	sort.Slice(rules, func(i, j int) bool {
		ri := selectorLen(rules[i].PodSelector)
		rj := selectorLen(rules[j].PodSelector)
		return ri > rj
	})

	w.onChange(intpolicy.New(rules))
}

// ── helpers ───────────────────────────────────────────────────────────────────

func selectorLen(sel labels.Selector) int {
	if sel == nil {
		return 0
	}
	reqs, _ := sel.Requirements()
	return len(reqs)
}

func unstructuredToPolicy(obj map[string]any) (v1alpha1.CachePolicy, error) {
	b, err := json.Marshal(obj)
	if err != nil {
		return v1alpha1.CachePolicy{}, err
	}
	var cp v1alpha1.CachePolicy
	if err := json.Unmarshal(b, &cp); err != nil {
		return v1alpha1.CachePolicy{}, err
	}
	return cp, nil
}

// PolicyForPod returns the best-matching policy rules for a pod with the
// given namespace and labels. Returns nil if no policy applies.
func (w *Watcher) PolicyForPod(namespace string, podLabels map[string]string) []intpolicy.Rule {
	w.mu.RLock()
	cps := append([]v1alpha1.CachePolicy{}, w.polMap[namespace]...)
	w.mu.RUnlock()

	set := labels.Set(podLabels)
	var matched []v1alpha1.CachePolicy
	for _, cp := range cps {
		sel, err := metav1.LabelSelectorAsSelector(&cp.Spec.PodSelector)
		if err != nil {
			continue
		}
		if sel.Matches(set) {
			matched = append(matched, cp)
		}
	}
	if len(matched) == 0 {
		return nil
	}

	// Pick the most specific match; break ties by name.
	sort.Slice(matched, func(i, j int) bool {
		si, _ := metav1.LabelSelectorAsSelector(&matched[i].Spec.PodSelector)
		sj, _ := metav1.LabelSelectorAsSelector(&matched[j].Spec.PodSelector)
		ri := selectorLen(si)
		rj := selectorLen(sj)
		if ri != rj {
			return ri > rj
		}
		return matched[i].Name < matched[j].Name
	})

	cp := matched[0]
	rules := make([]intpolicy.Rule, 0, len(cp.Spec.Rules))
	sel, _ := metav1.LabelSelectorAsSelector(&cp.Spec.PodSelector)
	for _, r := range cp.Spec.Rules {
		rules = append(rules, intpolicy.Rule{
			Namespace:    namespace,
			PodSelector:  sel,
			Host:         r.Host,
			Port:         r.Port,
			Methods:      r.Methods,
			Paths:        r.Paths,
			TTL:          r.TTL.Duration,
			MaxBodyBytes: r.MaxBodyBytes,
			VaryHeaders:  r.VaryHeaders,
			CacheBody:    r.CacheBody,
		})
	}
	return rules
}
