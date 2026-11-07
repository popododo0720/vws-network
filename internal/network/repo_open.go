package network

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/popododo0720/vws-network/internal/domain"
)

// StoreConfig selects the Kubernetes store: ConfigMaps in Namespace, read
// through Kubeconfig, or the in-cluster ServiceAccount when it is empty. The
// Lease that picks the replica running the workers is in Namespace too.
type StoreConfig struct {
	Namespace  string
	Kubeconfig string
	// lease is the election timing; tests shorten it.
	lease leaseTiming
}

// checkStore refuses a Config that does not set exactly one store, or names
// a namespace Kubernetes cannot hold. Open runs it before anything opens.
func checkStore(cfg Config) error {
	if (cfg.DatabasePath == "") == (cfg.Store.Namespace == "") {
		return &domain.Error{Kind: domain.Invalid, Msg: "set exactly one of the database path and the store namespace"}
	}
	if cfg.Store.Namespace == "" {
		return nil
	}
	if errs := validation.IsDNS1123Label(cfg.Store.Namespace); len(errs) > 0 {
		return &domain.Error{
			Kind: domain.Invalid,
			Msg:  fmt.Sprintf("store namespace %q: %s", cfg.Store.Namespace, strings.Join(errs, "; ")),
		}
	}
	return nil
}

// openStore is the one place a checked Config becomes a repository and the
// elector of its one writer. SQLite has no elector: its file has one process.
func openStore(ctx context.Context, cfg Config) (repository, elector, error) {
	if cfg.DatabasePath != "" {
		repo, err := openSQLite(ctx, cfg.DatabasePath)
		return repo, nil, err
	}
	c, rc, err := storeClient(ctx, cfg.Store)
	if err != nil {
		return nil, nil, err
	}
	el, err := leaseElector(ctx, rest.CopyConfig(rc), cfg.Store.Namespace, cfg.Store.lease)
	if err != nil {
		return nil, nil, fmt.Errorf("openStore: %w", err)
	}
	repo, err := newKubeRepo(ctx, c, cfg.Store.Namespace)
	if err != nil {
		return nil, nil, err
	}
	return repo, el, nil
}

// storeClient is the client of the Kubernetes store, refused when its
// namespace cannot hold the store.
func storeClient(ctx context.Context, store StoreConfig) (client.Client, *rest.Config, error) {
	rc, err := restConfig("kubernetes store", store.Kubeconfig)
	if err != nil {
		return nil, nil, fmt.Errorf("storeClient: %w", err)
	}
	// One commit is several ConfigMap calls; the client-go default of 5 QPS
	// would hold every mutation for seconds.
	rc.QPS, rc.Burst = storeQPS, storeBurst
	c, err := client.New(rc, client.Options{})
	if err != nil {
		return nil, nil, fmt.Errorf("storeClient: %w", err)
	}
	if err := probeNamespace(ctx, c, store.Namespace); err != nil {
		return nil, nil, err
	}
	return c, rc, nil
}

const (
	storeQPS   = 100
	storeBurst = 200
)

// probeNamespace refuses a namespace that cannot hold the store. A list in a
// missing namespace succeeds, so this is a dry-run create.
func probeNamespace(ctx context.Context, c client.Client, ns string) error {
	ctx, cancel := callCtx(ctx)
	defer cancel()
	probe := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{GenerateName: "vws-net-probe-", Namespace: ns}}
	if err := retried(func() error { return c.Create(ctx, probe, client.DryRunAll) }); err != nil {
		return fmt.Errorf("store namespace %s: %w", ns, err)
	}
	return nil
}
