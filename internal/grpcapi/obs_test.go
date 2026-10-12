package grpcapi

import (
	"context"
	"net"
	"sort"
	"strings"
	"testing"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	networkv1 "github.com/popododo0720/vws-proto/gen/go/vws/network/v1"

	"github.com/popododo0720/vws-lib/errs"
	"github.com/popododo0720/vws-lib/obs"

	"github.com/popododo0720/vws-network/internal/domain"
)

const conflictMsg = `Vpc vpc-1 has id annotation "", want "vpc-1"`

func conflictErr() error {
	return toStatus(&domain.Error{Kind: domain.Conflict, Msg: conflictMsg}, "")
}

type obsVPCs struct {
	networkv1.UnimplementedVpcServiceServer
}

func (obsVPCs) GetVpc(context.Context, *networkv1.GetVpcRequest) (*networkv1.Vpc, error) {
	return nil, conflictErr()
}

func (obsVPCs) CreateVpc(context.Context, *networkv1.CreateVpcRequest) (*longrunningpb.Operation, error) {
	panic("password=hunter2")
}

func (obsVPCs) DeleteVpc(context.Context, *networkv1.DeleteVpcRequest) (*longrunningpb.Operation, error) {
	return nil, conflictErr()
}

var boomStream = grpc.ServiceDesc{
	ServiceName: "vws.test.Boom",
	HandlerType: (*any)(nil),
	Streams: []grpc.StreamDesc{{
		StreamName:    "Watch",
		ServerStreams: true,
		Handler:       func(any, grpc.ServerStream) error { panic("password=hunter2") },
	}},
}

type obsServer struct {
	conn   *grpc.ClientConn
	reader *sdkmetric.ManualReader
}

func startObs(t *testing.T) obsServer {
	t.Helper()
	ctx := context.Background()
	reader := sdkmetric.NewManualReader()
	shutdown, err := obs.Setup(ctx, obs.ForService(ServiceName), obs.WithOverride(func(c *obs.Config) {
		c.MetricReaders = append(c.MetricReaders, reader)
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	lis, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer(ServerOptions()...)
	networkv1.RegisterVpcServiceServer(gs, obsVPCs{})
	gs.RegisterService(&boomStream, struct{}{})
	go func() {
		if err := gs.Serve(lis); err != nil {
			t.Error(err)
		}
	}()
	t.Cleanup(gs.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	})
	return obsServer{conn: conn, reader: reader}
}

// points returns "k=v,k=v value" for every point of the named int64 sum.
func (s obsServer) points(t *testing.T, name string) []string {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := s.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s data %T", name, m.Data)
			}
			for _, dp := range sum.DataPoints {
				var kv []string
				for _, a := range dp.Attributes.ToSlice() {
					kv = append(kv, string(a.Key)+"="+a.Value.AsString())
				}
				out = append(out, strings.Join(kv, ","))
			}
		}
	}
	sort.Strings(out)
	return out
}

func TestServerOptionsWithGrpcobs(t *testing.T) {
	s := startObs(t)
	vpcs := networkv1.NewVpcServiceClient(s.conn)

	_, err := vpcs.GetVpc(t.Context(), &networkv1.GetVpcRequest{})
	st := status.Convert(err)
	if st.Code() != codes.AlreadyExists || st.Message() != conflictMsg {
		t.Fatalf("conflict status %v %q, want AlreadyExists %q", st.Code(), st.Message(), conflictMsg)
	}
	if n := strings.Count(err.Error(), "rpc error"); n != 1 {
		t.Fatalf("conflict message wrapped %d times: %q", n, err.Error())
	}
	_, reason := statusInfo(&domain.Error{Kind: domain.Conflict}, "")
	var reasons []string
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			reasons = append(reasons, info.GetReason())
		}
	}
	if reason == "" || len(reasons) != 1 || reasons[0] != reason {
		t.Fatalf("ErrorInfo reasons %v, want [%s]", reasons, reason)
	}
	if got := errs.CodeOf(conflictErr()); got != errs.CodeAlreadyExists {
		t.Fatalf("errs.CodeOf(handler error) = %v, want AlreadyExists", got)
	}
	if got := errs.CodeOf(errs.FromGRPC(err)); got != errs.CodeAlreadyExists {
		t.Fatalf("errs.CodeOf(client error) = %v, want AlreadyExists", got)
	}
	if got := s.points(t, obs.MetricErrors); len(got) != 0 {
		t.Fatalf("conflict reported as a server error: vws.errors %v", got)
	}

	_, err = vpcs.CreateVpc(t.Context(), &networkv1.CreateVpcRequest{})
	if st := status.Convert(err); st.Code() != codes.Internal || st.Message() != "internal error" {
		t.Fatalf("unary panic status %v %q, want Internal \"internal error\"", st.Code(), st.Message())
	}
	stream, err := s.conn.NewStream(t.Context(), &boomStream.Streams[0], "/vws.test.Boom/Watch")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	err = stream.RecvMsg(&struct{}{})
	if st := status.Convert(err); st.Code() != codes.Internal || st.Message() != "internal error" {
		t.Fatalf("stream panic status %v %q, want Internal \"internal error\"", st.Code(), st.Message())
	}
	if got := s.points(t, obs.MetricPanics); len(got) != 1 {
		t.Fatalf("vws.panics points %v, want one", got)
	}

	_, err = vpcs.DeleteVpc(t.Context(), &networkv1.DeleteVpcRequest{})
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("delete status %v", err)
	}
	want := []string{
		"action=CreateVpc,outcome=internal,service=network",
		"action=DeleteVpc,outcome=alreadyexists,service=network",
	}
	if got := s.points(t, obs.MetricAuditEvents); strings.Join(got, ";") != strings.Join(want, ";") {
		t.Fatalf("vws.audit.events %v, want %v", got, want)
	}
}

func TestAuditedMethods(t *testing.T) {
	for m, want := range map[string]bool{"CreateVpc": true, "DeleteSubnet": true, "AuthorizeSecurityGroupIngress": true, "GetVpc": false, "ListOperations": false} {
		if isMutating(m) != want {
			t.Errorf("isMutating(%s) = %v, want %v", m, !want, want)
		}
	}
}
