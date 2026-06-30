package watcher

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

// Standard data keys in a kubernetes.io/tls Secret.
const (
	tlsCertKey = "tls.crt" // leaf certificate plus any intermediate chain
	tlsKeyKey  = "tls.key" // private key
	tlsCAKey   = "ca.crt"  // optional CA certificate
)

// TLSCertData holds the raw PEM bytes read from a kubernetes.io/tls Secret.
// Unlike values Secrets, the bytes are kept verbatim (not YAML-unmarshalled),
// because PEM blocks must be passed to varnishd unchanged.
type TLSCertData struct {
	Cert []byte // tls.crt (leaf + intermediate chain)
	Key  []byte // tls.key (private key)
	CA   []byte // ca.crt (optional; may be nil)
}

// Empty reports whether the certificate or private key is missing, which means
// no usable certificate is present (e.g. the Secret is absent or half-written).
func (d TLSCertData) Empty() bool {
	return len(d.Cert) == 0 || len(d.Key) == 0
}

// equal reports whether two TLSCertData carry identical bytes.
func (d TLSCertData) equal(o TLSCertData) bool {
	return bytes.Equal(d.Cert, o.Cert) && bytes.Equal(d.Key, o.Key) && bytes.Equal(d.CA, o.CA)
}

// TLSCertWatcher watches a single kubernetes.io/tls Secret by name and emits its
// certificate material whenever it changes. It mirrors [SecretWatcher] but emits
// raw PEM bytes instead of YAML-decoded values.
type TLSCertWatcher struct {
	clientset  kubernetes.Interface
	namespace  string
	secretName string
	ch         chan TLSCertData

	mu       sync.Mutex
	previous TLSCertData
	synced   bool
}

// NewTLSCertWatcher creates a new TLSCertWatcher.
func NewTLSCertWatcher(clientset kubernetes.Interface, namespace, name string) *TLSCertWatcher {
	return &TLSCertWatcher{
		clientset:  clientset,
		namespace:  namespace,
		secretName: name,
		ch:         make(chan TLSCertData, 1),
	}
}

// Changes returns the channel on which TLS certificate updates are delivered.
func (w *TLSCertWatcher) Changes() <-chan TLSCertData {
	return w.ch
}

// Run starts watching the Secret and blocks until ctx is cancelled.
//
//nolint:dupl // Mirrors SecretWatcher.Run; the two emit different types and cannot share code.
func (w *TLSCertWatcher) Run(ctx context.Context) error {
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

	slog.Info("watching Secret for TLS certificate", "namespace", w.namespace, "secret", w.secretName)
	<-ctx.Done()

	return nil
}

func (w *TLSCertWatcher) sync(lister corelisters.SecretLister) {
	// Hold the mutex across the lister read and the send so the two are atomic:
	// the explicit sync in Run can run concurrently with an informer-handler
	// sync, and serialising read+send guarantees the last sender is the last
	// reader, so a stale value can't win the cap-1 channel. See Watcher.sync.
	w.mu.Lock()
	defer w.mu.Unlock()

	secret, err := lister.Secrets(w.namespace).Get(w.secretName)
	if err != nil {
		// Secret not found - emit empty data.
		w.sendLocked(TLSCertData{})

		return
	}

	w.sendLocked(TLSCertData{
		Cert: secret.Data[tlsCertKey],
		Key:  secret.Data[tlsKeyKey],
		CA:   secret.Data[tlsCAKey],
	})
}

// sendLocked performs the dedup and drain-then-send. Callers must hold w.mu.
func (w *TLSCertWatcher) sendLocked(data TLSCertData) {
	if w.synced && data.equal(w.previous) {
		return
	}

	w.synced = true
	w.previous = data

	coalescingSend(w.ch, data)
}
