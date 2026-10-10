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
	flag.Parse()
	if *dbPath == "" || *poolFlag == "" {
		fmt.Fprintln(os.Stderr, "usage: vws-network -db PATH -pool CIDR [-listen ADDR]")
		os.Exit(2)
	}
	pool, err := netip.ParsePrefix(*poolFlag)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	svc, err := network.Open(ctx, network.Config{DatabasePath: *dbPath, AddressPool: pool})
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = svc.Close() }()

	lis, err := net.Listen("tcp", *listen)
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
