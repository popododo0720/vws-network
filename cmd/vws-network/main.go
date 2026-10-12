// Command vws-network serves the VWS network control API.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"

	"github.com/popododo0720/vws-lib/obs"
	"github.com/popododo0720/vws-lib/obs/health"

	"github.com/popododo0720/vws-network/internal/grpcapi"
	"github.com/popododo0720/vws-network/internal/network"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "grpc listen address")
	dbPath := flag.String("db", "", "sqlite database path")
	poolFlag := flag.String("pool", "", "elastic ip pool cidr")
	kubeconfig := flag.String("kubeconfig", "", "kubeconfig path; selects the Kube-OVN dataplane and takes precedence over the in-cluster config")
	externalSubnet := flag.String("kube-external-subnet", "", "Kube-OVN subnet that holds the elastic IP pool; required for Kube-OVN, and without -kubeconfig it selects the in-cluster service account")
	nadNamespace := flag.String("kube-nad-namespace", "", "namespace of the per-subnet NetworkAttachmentDefinitions; defaults to vws-system when Kube-OVN is selected")
	snat := flag.Bool("kube-snat", false, "give every subnet outbound SNAT through -kube-external-subnet")
	flag.Parse()
	if *dbPath == "" || *poolFlag == "" {
		fmt.Fprintln(os.Stderr, "usage: vws-network -db PATH -pool CIDR [-listen ADDR] [-kubeconfig PATH] [-kube-external-subnet NAME [-kube-nad-namespace NS] [-kube-snat]]")
		os.Exit(2)
	}
	pool, err := netip.ParsePrefix(*poolFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "-pool:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *listen, *dbPath, pool, network.KubeOVNConfig{
		Kubeconfig:     *kubeconfig,
		ExternalSubnet: *externalSubnet,
		NADNamespace:   *nadNamespace,
		SNAT:           *snat,
	}); err != nil {
		slog.Default().LogAttrs(ctx, slog.LevelError, "vws-network stopped", slog.String(obs.FieldErr, err.Error()))
		stop()
		os.Exit(1)
	}
}

func run(ctx context.Context, listen, dbPath string, pool netip.Prefix, kube network.KubeOVNConfig) (err error) {
	shutdown, err := obs.Setup(ctx, obs.ForService(grpcapi.ServiceName))
	if err != nil {
		return fmt.Errorf("obs setup: %w", err)
	}
	defer func() {
		if serr := shutdown(context.WithoutCancel(ctx)); serr != nil {
			err = errors.Join(err, fmt.Errorf("obs shutdown: %w", serr))
		}
	}()
	svc, err := network.Open(ctx, network.Config{DatabasePath: dbPath, AddressPool: pool, KubeOVN: kube})
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer func() {
		if cerr := svc.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("close: %w", cerr))
		}
	}()
	health.Default().Register("network", svc.Ready)

	lis, err := (&net.ListenConfig{}).Listen(ctx, "tcp", listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	srv := grpc.NewServer(grpcapi.ServerOptions()...)
	grpcapi.Register(srv, svc)
	grpcHealth := grpcapi.RegisterHealth(srv, svc.Ready)
	go grpcHealth.Run(ctx, 5*time.Second)
	go func() {
		<-ctx.Done()
		health.Default().NotReady()
		grpcHealth.Shutdown()
		srv.GracefulStop()
	}()
	if err := srv.Serve(lis); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}
