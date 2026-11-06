package network

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
)

func TestOpenSelectsStoreFromConfig(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "a.db")
	open := func(db string, store StoreConfig) error {
		svc, err := Open(ctx, Config{DatabasePath: db, Store: store, AddressPool: mustP("198.51.100.0/24")})
		if err == nil {
			closeOrFail(t, svc.Close)
		}
		return err
	}
	for name, store := range map[string]StoreConfig{
		"neither":          {},
		"uppercase":        {Namespace: "VWS"},
		"path":             {Namespace: "../kube-system"},
		"too long":         {Namespace: strings.Repeat("a", 64)},
		"leading hyphen":   {Namespace: "-vws"},
		"control byte":     {Namespace: "vws\n"},
		"kubeconfig only":  {Kubeconfig: writeKubeconfig(t)},
		"both with sqlite": {Namespace: "vws-system"},
	} {
		path := ""
		if name == "both with sqlite" {
			path = db
		}
		if err := open(path, store); !isInvalid(err) {
			t.Errorf("%s: err %v, want Invalid", name, err)
		}
	}
	stubInCluster(t, "", rest.ErrNotInCluster)
	err := open("", StoreConfig{Namespace: "vws-system"})
	if !errors.Is(err, rest.ErrNotInCluster) || !strings.Contains(err.Error(), "kubernetes store needs -kubeconfig") {
		t.Fatalf("store outside a pod: err %v, want ErrNotInCluster", err)
	}
	absent := filepath.Join(t.TempDir(), "absent")
	if err := open("", StoreConfig{Namespace: "vws-system", Kubeconfig: absent}); err == nil || isInvalid(err) {
		t.Fatalf("absent kubeconfig: err %v", err)
	}
	// repair lists the store at Open, so an unreachable API server fails the
	// start instead of serving without state.
	err = open("", StoreConfig{Namespace: "vws-system", Kubeconfig: writeKubeconfig(t)})
	if err == nil || !strings.Contains(err.Error(), "list project") {
		t.Fatalf("unreachable store: err %v", err)
	}
	svc, err := Open(ctx, Config{DatabasePath: db, AddressPool: mustP("198.51.100.0/24")})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, svc.Close)
	if _, ok := svc.repo.(*sqliteRepo); !ok {
		t.Fatalf("database path gave %T", svc.repo)
	}
}
