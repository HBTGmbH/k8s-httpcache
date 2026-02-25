package watcher

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/yaml"
)

// SecretWatcher watches a single Secret by name and emits its data
// whenever it changes. Each byte-slice value is converted to a string
// and YAML-unmarshalled into an arbitrary type so that structured data
// (maps, lists, numbers) is accessible in templates.
type SecretWatcher struct {
	clientset  kubernetes.Interface
	namespace  string
	secretName string
	ch         chan map[string]any

	mu       sync.Mutex
	previous map[string]any
	synced   bool
}

// NewSecretWatcher creates a new SecretWatcher.
func NewSecretWatcher(clientset kubernetes.Interface, namespace, name string) *SecretWatcher {
	return &SecretWatcher{
		clientset:  clientset,
		namespace:  namespace,
		secretName: name,
		ch:         make(chan map[string]any, 1),
	}
}

// Changes returns the channel on which Secret data updates are delivered.
func (w *SecretWatcher) Changes() <-chan map[string]any {
	return w.ch
}

// Run starts watching the Secret and blocks until ctx is cancelled.
//
//nolint:dupl // Mirrors ConfigMapWatcher.Run; the two use different API types and cannot share code.
func (w *SecretWatcher) Run(ctx context.Context) error {
	factory := informers.NewSharedInformerFactoryWithOptions(
		w.clientset,
		0,
		informers.WithNamespace(w.namespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.FieldSelector = fields.OneTermEqualSelector("metadata.name", w.secretName).String()
		}),
	)

	informer := factory.Core().V1().Secrets().Informer()
	lister := factory.Core().V1().Secrets().Lister()

	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(_ any) { w.sync(lister) },
		UpdateFunc: func(_, _ any) { w.sync(lister) },
		DeleteFunc: func(_ any) { w.sync(lister) },
	}

	_, err := informer.AddEventHandler(handler)
	if err != nil {
		return fmt.Errorf("adding event handler: %w", err)
	}

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())

	// Deliver initial state even if no Secret exists.
	w.sync(lister)

	slog.Info("watching Secret for values", "namespace", w.namespace, "secret", w.secretName)
	<-ctx.Done()

	return nil
}

func (w *SecretWatcher) sync(lister corelisters.SecretLister) {
	secret, err := lister.Secrets(w.namespace).Get(w.secretName)
	if err != nil {
		// Secret not found — emit empty data.
		w.send(nil)

		return
	}

	parsed := make(map[string]any, len(secret.Data))
	for k, v := range secret.Data {
		var val any
		err := yaml.Unmarshal(v, &val)
		if err != nil {
			val = string(v) // fallback to raw string on parse error
		}
		parsed[k] = val
	}

	w.send(parsed)
}

func (w *SecretWatcher) send(data map[string]any) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.synced && reflect.DeepEqual(data, w.previous) {
		return
	}

	w.synced = true
	w.previous = data

	// Non-blocking send: drain then send.
	select {
	case <-w.ch:
	default:
	}
	w.ch <- data
}
