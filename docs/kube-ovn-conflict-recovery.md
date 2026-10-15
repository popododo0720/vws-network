# Kube-OVN conflict recovery

The operating procedure for when the Kube-OVN dataplane meets an existing object with the same name and returns `domain.Conflict`. It was written by reading `ensureObject`, `remove`, `removeObject` and `deleteSubnet` in `internal/network/dataplane_kubeovn.go`, `failOrRetry` in `internal/network/service.go`, and `Fail` and `Plan` in `internal/domain/state.go`. It has not been followed on a real Kube-OVN cluster. Not verified.

## When a conflict happens

The object name is the resource id, and an id is a prefix such as `vpc-` or `subnet-` followed by 8 random bytes in hex. A conflict happens only when someone created an object with the same name beforehand. The affected kinds are Vpc, Subnet, SecurityGroup, OvnEip, OvnSnatRule and NetworkAttachmentDefinition.

`ensureObject` tries to create first; if the object already exists, it reads it and checks in this order:

1. Being deleted (`deletionTimestamp` set): it returns an ordinary error and the operation is retried. This is not a conflict.
2. The annotation `vws.popododo0720.io/id` is missing or differs from the id: `domain.Conflict`.
3. Any spec field we write differs from the desired value: `domain.Conflict`. The rule lists of security groups and the host subnet of NAT gateways are not compared.

A reboot or restart does not cause conflicts. A restart returns `RUNNING` operations to `PENDING` (`repository.requeue`), and the ensure that runs again treats objects we made (same id annotation and spec) as fine. A conflict happens only when another object with the same name exists or someone changed the annotation or spec of our object.

## What the code does on a conflict

`failOrRetry` does not retry `domain.Invalid` and `domain.Conflict`; it ends the operation as `FAILED` at once. The error message carries the object kind, name, expected value and actual value. Over gRPC it shows as `AlreadyExists` (`ALREADY_EXISTS`).

The resource state depends on the operation kind:

- A resource being created (`PENDING`) becomes `FAILED`. `Plan` makes no ensure for a `FAILED` resource and also blocks children under a `FAILED` parent. There is no way to converge the same id again.
- On a conflict while updating an existing resource, the resource stays as it is and only the operation is `FAILED`.

## Recovery

### An update operation conflicted

1. Look at the object the error message names with `kubectl get <kind> <name> -o yaml`.
2. If the annotation is missing, restore it with `kubectl annotate <kind> <name> vws.popododo0720.io/id=<name>`. If the spec changed, restore the expected value from the message.
3. Send the same update again with a new `idempotency_key`. The same key just returns the original operation and has no effect.

### A create operation conflicted and the resource is `FAILED`

The only recovery the code supports is deleting the `FAILED` resource and creating it again with a new id.

`remove` and `removeObject` read the object and delete it only when the annotation `vws.popododo0720.io/id` equals the resource id. If the annotation is missing or different, they refuse with `domain.Conflict` and delete nothing. The delete carries the UID of the object read as a precondition, so an object replaced under the same name after the read is not deleted. A missing object is success. So deleting a `FAILED` resource never deletes someone's object of the same name; instead that delete operation ends `FAILED` with `Conflict`.

Even when the delete operation fails, the resource stays `DELETING`, because `FailKind` turns only `PENDING` resources into `FAILED`. A subnet delete removes the SNAT rule, the SNAT address, the NAT gateway that subnet hosted, the Subnet and the NAD in that order, so if a later step conflicts, our objects from the earlier steps are already gone.

1. Look at the object the error message names with `kubectl get <kind> <name> -o yaml`.
2. If the object is needed, back up its content and create it again under another name (object names cannot be changed). Then delete the object with the original name with `kubectl delete`. If it is not needed, delete it right away. The code does not delete this object, so a person has to.
3. Send the same delete again with a new `idempotency_key`. The object is gone, so the delete finishes. If dependent resources (subnets, addresses and so on) exist, the delete is refused first.
4. When the delete is done, send a new `CreateXxx` request to get a new id.

## What the code does not support

- There is no automatic adopt. An object without the annotation is never taken over as ours. Even if someone adds the annotation, a create that is already `FAILED` does not come back.
- There is no retry RPC that converges a `FAILED` resource again.
- A conflicting object is not overwritten with our spec. Create does not overwrite, and existing objects are only read.
- Nothing deletes or moves, on your behalf, an object whose id annotation is missing or different. A person must clean it up with `kubectl`.

## OvnEips created with v4ip (vws-test 38, 38b)

Before this fix, vws-network wrote `spec.v4ip` (lower case) to OvnEips. The Kube-OVN v1.16.12 CRD field is `v4Ip`, so the API server dropped the field and Kube-OVN picked another address in the external Subnet (the API said 10.78.0.225, the actual OvnEip was 10.78.0.201). After the fix, ensuring the same OvnEip again gives `domain.Conflict` because `spec.v4Ip` differs. It is not overwritten. If the address has no OvnFip, delete the OvnEip and run the operation again. If it has an OvnFip, delete the OvnFip first.

## VpcNatGateways created by the previous release

`-kube-snat` now uses only OVN-native SNAT (OvnEip + OvnSnatRule). The previous release created an iptables `VpcNatGateway` `nat-<vpc id>` per VPC, and its pod could not start because kube-system had no NAD named after the external Subnet (vws-test 40, 41). The service does not delete these objects. The operator checks the id annotation (`vws.popododo0720.io/id: nat-<vpc id>`) and deletes it with `kubectl delete vpc-nat-gateway nat-<vpc id>`.
