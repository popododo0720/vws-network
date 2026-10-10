package grpcapi

import (
	"context"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	commonv1 "github.com/popododo0720/vws-proto/gen/go/vws/common/v1"

	"github.com/popododo0720/vws-network/internal/domain"
	"github.com/popododo0720/vws-network/internal/network"
)

func (h *handler) toOp(project, requestID, resourceName string, verb commonv1.OperationVerb, op domain.Operation) (*longrunningpb.Operation, error) {
	meta, err := anypb.New(&commonv1.OperationMetadata{
		Resource:  resourceName,
		Verb:      verb,
		RequestId: requestID,
	})
	if err != nil {
		return nil, status.Error(codes.Internal, "internal error")
	}
	out := &longrunningpb.Operation{
		Name:     "projects/" + project + "/operations/" + op.ID,
		Metadata: meta,
	}
	switch op.State {
	case domain.OpSucceeded:
		out.Done = true
	case domain.OpFailed:
		out.Done = true
		msg := "failed"
		code := codes.Unavailable
		if op.Err != nil {
			msg = op.Err.Msg
			code, _ = statusInfo(op.Err, requestID)
		}
		out.Result = &longrunningpb.Operation_Error{Error: status.New(code, msg).Proto()}
	}
	return out, nil
}

func verbOf(typ domain.OperationType) commonv1.OperationVerb {
	switch typ {
	case domain.OpCreateVPC, domain.OpCreateSubnet, domain.OpCreateSecurityGroup, domain.OpAllocateElasticIP:
		return commonv1.OperationVerb_OPERATION_VERB_CREATE
	case domain.OpDeleteVPC, domain.OpDeleteSubnet, domain.OpDeleteSecurityGroup, domain.OpReleaseElasticIP:
		return commonv1.OperationVerb_OPERATION_VERB_DELETE
	default:
		return commonv1.OperationVerb_OPERATION_VERB_UPDATE
	}
}

func (h *handler) GetOperation(ctx context.Context, req *longrunningpb.GetOperationRequest) (*longrunningpb.Operation, error) {
	if req == nil {
		return nil, requiredRequest()
	}
	project, id, err := parseOpName(req.GetName())
	if err != nil {
		return nil, err
	}
	view, err := h.svc.ViewOperation(ctx, project, id)
	if err != nil {
		return nil, toStatus(err, "")
	}
	op := view.Operation
	resourceName, msg := operationMessage(string(project), view)
	requestID := string(op.Key)
	if !uuid4(requestID) {
		requestID = ""
	}
	out, err := h.toOp(string(project), requestID, resourceName, verbOf(op.Type), op)
	if err != nil {
		return nil, err
	}
	if op.State == domain.OpSucceeded && msg != nil {
		body, err := anypb.New(msg)
		if err != nil {
			return nil, status.Error(codes.Internal, "internal error")
		}
		out.Result = &longrunningpb.Operation_Response{Response: body}
	}
	return out, nil
}

func operationMessage(project string, view network.OperationView) (string, proto.Message) {
	switch {
	case view.VPC != nil:
		msg := toVPC(project, *view.VPC)
		return msg.GetName(), msg
	case view.Subnet != nil:
		msg := toSubnet(project, *view.Subnet)
		return msg.GetName(), msg
	case view.Group != nil:
		msg := toGroup(project, *view.Group)
		return msg.GetName(), msg
	case view.ElasticIP != nil:
		msg := toAddress(project, view.ElasticIP.Region, *view.ElasticIP)
		return msg.GetName(), msg
	default:
		return "", nil
	}
}
