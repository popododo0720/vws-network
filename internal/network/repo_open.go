package network

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/popododo0720/vws-network/internal/domain"
)

// StoreConfig selects the Kubernetes store: ConfigMaps in Namespace, read
// through Kubeconfig, or the in-cluster ServiceAccount when it is empty.
type StoreConfig struct {
	Namespace  string
	Kubeconfig string
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

// openStore is the one place a checked Config becomes a repository.
func openStore(ctx context.Context, cfg Config) (repository, error) {
	if cfg.DatabasePath != "" {
		return openSQLite(ctx, cfg.DatabasePath)
	}
	rc, err := restConfig("kubernetes store", cfg.Store.Kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("openStore: %w", err)
	}
	// One commit is several ConfigMap calls; the client-go default of 5 QPS
	// would hold every mutation for seconds.
	rc.QPS, rc.Burst = storeQPS, storeBurst
	c, err := client.New(rc, client.Options{})
	if err != nil {
		return nil, fmt.Errorf("openStore: %w", err)
	}
	if err := probeNamespace(ctx, c, cfg.Store.Namespace); err != nil {
		return nil, err
	}
	return newKubeRepo(ctx, c, cfg.Store.Namespace)
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
