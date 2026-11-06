package network

import (
	"context"
	"fmt"
	"strings"

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
	c, err := client.New(rc, client.Options{})
	if err != nil {
		return nil, fmt.Errorf("openStore: %w", err)
	}
	return newKubeRepo(ctx, c, cfg.Store.Namespace)
}
