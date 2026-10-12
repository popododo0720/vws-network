# Health

vws-network serves `grpc.health.v1.Health` on the same port as the API, the `-listen` address. A Kubernetes gRPC probe must use that port.

Names: `""`, `google.longrunning.Operations`, `vws.network.v1.AddressService`, `vws.network.v1.SecurityGroupService`, `vws.network.v1.SubnetService`, `vws.network.v1.VpcService`. They all share one status.

The status is SERVING when all three of these hold. The check runs every 5 seconds with a 2 second budget.

1. SQLite answers `SELECT 1`.
2. A worker loop went round within the last minute.
3. The dataplane answers a read. The fake always does. Kube-OVN must return the `-kube-external-subnet` Subnet.

An operation backlog or failed operations never make the service NOT_SERVING. On SIGTERM every name turns NOT_SERVING before `GracefulStop`, and stays that way.

Use the readiness probe on `""`. Keep liveness on `tcpSocket` so a Kube-OVN API outage does not restart the pod.

Not verified: whether the kubelet gRPC probe works against the chart's `-listen` binding, and how readiness behaves on real Kube-OVN. Both need a cluster (T1/T2).
