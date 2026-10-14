# VM MTU (#93, vws-deploy#236)

`-vm-mtu` (default 9001, the AWS VPC MTU; 68–9001 or the start fails with Invalid) is the MTU of
VM interfaces in VPC subnets. The chart reads it from VWSCluster `spec.network.vmMTU`; the
installer lowers that field when the node NIC cannot carry it. vws-network does not read
VWSCluster and does not compute anything (`underlayMTU`, `tunnelOverhead`, `ipv4Only` are the
platform's). The flag name lives in one constant, `flagVMMTU` in `cmd/vws-network/main.go`.

## What vws-network writes

Every VPC Subnet it makes gets:

| field | value | Kube-OVN v1.16.12 |
| --- | --- | --- |
| `spec.mtu` | `-vm-mtu` | the pod interface MTU (`pkg/daemon/handler.go:369`), which KubeVirt passes to the VM, and the DHCP mtu when the options string has none (`pkg/controller/subnet.go:446 getSubnetMTU`) |
| `spec.enableDHCP` | `true` | makes the OVN DHCP_Options row for the subnet |
| `spec.dhcpV4Options` | `lease_time=3600,mtu=<-vm-mtu>` | an options string, not a UUID as the CRD text says. Kube-OVN adds the missing `router`, `server_id`, `server_mac` (`pkg/ovs/util.go buildDHCPv4Options`) |

Subnets that already exist are not changed: `ensure` creates and never updates a spec.
Direct (underlay) subnets and the external subnet keep the physical network's MTU.

## The 1500 path out (EIP, SNAT, external network)

Kube-OVN v1.16.12 sets no OVN `gateway_mtu` on any logical router port and has no Subnet or Vpc
field for it (no `gateway_mtu` or `check_pkt_larger` anywhere in `pkg/`). So the VPC router does
not answer an oversized packet bound for the 1500 external network with ICMP "fragmentation
needed": a packet over 1500 with DF set is dropped where the 1500 link is, and PMTUD does not
work through the router. TCP does not get an MSS clamp either.

vws-network only talks to the Kubernetes API, so it cannot set the OVN NB option itself. Until
Kube-OVN exposes it, the fix is outside this repo:

- set `options:gateway_mtu=<external MTU>` on each VPC router's port to the external subnet
  (`ovn-nbctl set logical_router_port <vpc>-<external> options:gateway_mtu=1500`), from a
  platform component that has OVN NB access, or
- keep `-vm-mtu` at the external MTU where VMs must reach the outside with large packets.

The overlay itself needs the underlay MTU to be at least `-vm-mtu` plus the tunnel overhead
(Geneve 100); that is the installer's preflight.
