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

// ConfigMapWatcher watches a single ConfigMap by name and emits its data
// whenever it changes. Each string value is YAML-unmarshalled into an
// arbitrary type so that structured data (maps, lists, numbers) is
// accessible in templates.
type ConfigMapWatcher struct {
	clientset     kubernetes.Interface
	namespace     string
	configMapName string
	ch            chan map[string]any

	mu       sync.Mutex
	previous map[string]any
	synced   bool
}

// NewConfigMapWatcher creates a new ConfigMapWatcher.
func NewConfigMapWatcher(clientset kubernetes.Interface, namespace, name string) *ConfigMapWatcher {
	return &ConfigMapWatcher{
		clientset:     clientset,
		namespace:     namespace,
		configMapName: name,
		ch:            make(chan map[string]any, 1),
	}
}

// Changes returns the channel on which ConfigMap data updates are delivered.
func (w *ConfigMapWatcher) Changes() <-chan map[string]any {
	return w.ch
}

// Run starts watching the ConfigMap and blocks until ctx is cancelled.
func (w *ConfigMapWatcher) Run(ctx context.Context) error {
	factory := informers.NewSharedInformerFactoryWithOptions(
		w.clientset,
		0,
		informers.WithNamespace(w.namespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.FieldSelector = fields.OneTermEqualSelector("metadata.name", w.configMapName).String()
		}),
	)

	informer := factory.Core().V1().ConfigMaps().Informer()
	lister := factory.Core().V1().ConfigMaps().Lister()

	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(_ any) { w.sync(lister) },
		UpdateFunc: func(_, _ any) { w.sync(lister) },
		DeleteFunc: func(_ any) { w.sync(lister) },
	}

	if _, err := informer.AddEventHandler(handler); err != nil {
		return fmt.Errorf("adding event handler: %w", err)
	}

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())

	// Deliver initial state even if no ConfigMap exists.
	w.sync(lister)

	slog.Info("watching ConfigMap for values", "namespace", w.namespace, "configmap", w.configMapName)
	<-ctx.Done()
	return nil
}

func (w *ConfigMapWatcher) sync(lister corelisters.ConfigMapLister) {
	cm, err := lister.ConfigMaps(w.namespace).Get(w.configMapName)
	if err != nil {
		// ConfigMap not found — emit empty data.
		w.send(nil)
		return
	}

	parsed := make(map[string]any, len(cm.Data))
	for k, v := range cm.Data {
		var val any
		if err := yaml.Unmarshal([]byte(v), &val); err != nil {
			val = v // fallback to raw string on parse error
		}
		parsed[k] = val
	}

	w.send(parsed)
}

func (w *ConfigMapWatcher) send(data map[string]any) {
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
