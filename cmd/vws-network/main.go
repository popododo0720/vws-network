// Command vws-network serves the VWS network control API.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"

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
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	svc, err := network.Open(ctx, network.Config{
		DatabasePath: *dbPath,
		AddressPool:  pool,
		KubeOVN: network.KubeOVNConfig{
			Kubeconfig:     *kubeconfig,
			ExternalSubnet: *externalSubnet,
			NADNamespace:   *nadNamespace,
			SNAT:           *snat,
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := svc.Close(); err != nil {
			log.Printf("close: %v", err)
		}
	}()

	lis, err := (&net.ListenConfig{}).Listen(ctx, "tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	srv := grpc.NewServer()
	grpcapi.Register(srv, svc)
	go func() {
		<-ctx.Done()
		srv.GracefulStop()
	}()
	if err := srv.Serve(lis); err != nil {
		log.Fatal(err)
	}
}
