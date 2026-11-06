//go:build envtest

package network

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// storeEnv is one API server shared by every kube-envtest store. Each opened
// store gets its own namespace.
var storeEnv struct {
	once sync.Once
	env  *envtest.Environment
	c    client.Client
	err  error
	n    atomic.Int64
}

func init() {
	extraOpeners = append(extraOpeners, storeOpener{name: "kube-envtest", open: func(t *testing.T) repository {
		t.Helper()
		if os.Getenv("KUBEBUILDER_ASSETS") == "" {
			t.Skip("KUBEBUILDER_ASSETS is not set; install with setup-envtest")
		}
		storeEnv.once.Do(func() {
			storeEnv.env = &envtest.Environment{}
			cfg, err := storeEnv.env.Start()
			if err != nil {
				storeEnv.err = err
				return
			}
			storeEnv.c, storeEnv.err = client.New(cfg, client.Options{})
		})
		if storeEnv.err != nil {
			t.Fatal(storeEnv.err)
		}
		ns := fmt.Sprintf("store-%d", storeEnv.n.Add(1))
		if err := storeEnv.c.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
		return openKubeRepo(t, storeEnv.c, ns)
	}})
}

func TestMain(m *testing.M) {
	code := m.Run()
	if storeEnv.env != nil {
		if err := storeEnv.env.Stop(); err != nil {
			fmt.Fprintln(os.Stderr, "stop store envtest:", err)
			code = 1
		}
	}
	os.Exit(code)
}
