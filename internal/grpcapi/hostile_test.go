package grpcapi

import (
	"context"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	networkv1 "github.com/popododo0720/vws-proto/gen/go/vws/network/v1"

	"github.com/popododo0720/vws-network/internal/network"
)

func TestHostileInputIsInvalidArgument(t *testing.T) {
	ctx := context.Background()
	svc, err := network.Open(ctx, network.Config{
		DatabasePath: filepath.Join(t.TempDir(), "net.db"),
		AddressPool:  netip.MustParsePrefix("198.51.100.0/24"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, svc.Close)
	conn := serve(t, svc)
	vpcs := networkv1.NewVpcServiceClient(conn)
	parent := "projects/proj/regions/r1"

	cidrs := []string{
		"", "10.0.0.0", "10.0.0.0/16/16", "10.0.0.0/16; DROP TABLE projects", "10.0.0.0/-1",
		"10.0.0.0/33", "256.0.0.0/16", "::1/128", "10.0.0.0/16\x00", strings.Repeat("1", 4096),
	}
	for _, cidr := range cidrs {
		_, err := vpcs.CreateVpc(ctx, &networkv1.CreateVpcRequest{
			Parent: parent, VpcId: "app",
			Vpc: &networkv1.Vpc{Spec: &networkv1.VpcSpec{Ipv4Cidr: cidr}},
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("cidr %.40q gave code %s err %v, want InvalidArgument", cidr, status.Code(err), err)
		}
	}

	ids := []string{"", "UPPER", "a/b", "../x", "app\x00", "a b", strings.Repeat("a", 4096)}
	for _, id := range ids {
		_, err := vpcs.CreateVpc(ctx, &networkv1.CreateVpcRequest{
			Parent: parent, VpcId: id,
			Vpc: &networkv1.Vpc{Spec: &networkv1.VpcSpec{Ipv4Cidr: "10.0.0.0/16"}},
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("vpc id %.40q gave code %s err %v, want InvalidArgument", id, status.Code(err), err)
		}
	}

	list, err := vpcs.ListVpcs(ctx, &networkv1.ListVpcsRequest{Parent: parent})
	if err != nil || len(list.GetVpcs()) != 0 {
		t.Fatalf("rejected requests left %v err %v", list.GetVpcs(), err)
	}
}
