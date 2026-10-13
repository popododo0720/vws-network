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
	poolFlag := flag.String("pool", "", "elastic ip pool cidr; required, no default, and never an RFC 5737 documentation range")
	kubeconfig := flag.String("kubeconfig", "", "kubeconfig path; selects the Kube-OVN dataplane and takes precedence over the in-cluster config")
	externalSubnet := flag.String("kube-external-subnet", "", "Kube-OVN subnet that holds the elastic IP pool; required for Kube-OVN, and without -kubeconfig it selects the in-cluster service account")
	nadNamespace := flag.String("kube-nad-namespace", "", "namespace of the per-subnet NetworkAttachmentDefinitions; defaults to vws-system when Kube-OVN is selected")
	snat := flag.Bool("kube-snat", false, "give every subnet outbound OVN-native SNAT (OvnEip and OvnSnatRule, no VpcNatGateway) through -kube-external-subnet")
	providerNetwork := flag.String("kube-provider-network", "", "Kube-OVN ProviderNetwork to create for the external network (at most 12 characters); selects the underlay")
	underlayIface := flag.String("kube-underlay-interface", "", "node NIC the provider network binds, for example ens3; required with -kube-provider-network")
	vlanID := flag.Int("kube-underlay-vlan", 0, "VLAN tag of the external network: 0 is flat, 1-4094 tagged")
	underlayCIDR := flag.String("kube-underlay-cidr", "", "CIDR of the external network; must contain -pool")
	underlayGW := flag.String("kube-underlay-gateway", "", "gateway of the external network; inside -kube-underlay-cidr and outside -pool")
	underlayExclude := flag.String("kube-underlay-exclude", "", "comma-separated addresses or a..b ranges of the external network Kube-OVN must not hand out; the gateway is always excluded")
	directFlag := flag.String("kube-direct-networks", "", `direct networks as one JSON array line: [{"name","providerNetwork","interface","vlan","cidr","gateway","exclude"}]; each becomes Subnet and NAD vws-direct-<name> and the read-only API subnet subnet-direct-<name>`)
	directNADNS := flag.String("kube-direct-nad-namespace", "", "VM namespace where the direct network NADs vws-direct-<name> are made (vws-deploy#125); required with -kube-direct-networks, no default")
	flag.Parse()
	pool, err := parseStart(*dbPath, *poolFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vws-network:", err)
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	underlay, err := parseUnderlay(*providerNetwork, *underlayIface, *vlanID, *underlayCIDR, *underlayGW, *underlayExclude)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vws-network:", err)
		os.Exit(2)
	}
	direct, err := parseDirect(*directFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vws-network:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *listen, *dbPath, pool, network.KubeOVNConfig{
		Kubeconfig:         *kubeconfig,
		ExternalSubnet:     *externalSubnet,
		NADNamespace:       *nadNamespace,
		SNAT:               *snat,
		Underlay:           underlay,
		Direct:             direct,
		DirectNADNamespace: *directNADNS,
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
