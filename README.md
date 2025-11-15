# Gateway Yeeter

A Kubernetes MutatingAdmissionWebhook that removes problematic default gateway configurations from MTV (Migration Toolkit for Virtualization) migration pods to enable vCenter connectivity while preserving transfer network access.

## Problem

MTV's transfer network feature allows separating VM migration traffic from normal cluster traffic or leveraging high-speed layer 2 networks for disk transfers. In some deployments, the transfer network is a non-routable layer 2 network with direct ESXi host connectivity but no gateway, while vCenter remains accessible only via the cluster's default routing/egress.

When a NetworkAttachmentDefinition includes a gateway (via `forklift.konveyor.io/route` annotation or an `ipam.routes` entry for `0.0.0.0/0`/`::/0`), Forklift's [`setTransferNetwork()`](https://github.com/kubev2v/forklift/blob/main/pkg/controller/plan/kubevirt.go#L2804) uses the modern `k8s.v1.cni.cncf.io/networks` annotation with a `default-route` field. Without a gateway, it falls back to the legacy `v1.multus-cni.io/default-network` annotation.

**This creates a conflict for OVN-Kubernetes localnet topologies in non-routable transfer network scenarios:**

- **Legacy annotation path** (no gateway): OVN localnet cannot attach the network
- **Modern annotation path** (with gateway): Network attaches successfully, but the default route breaks vCenter connectivity

Both CDI importer pods (`app=containerized-data-importer`) and virt-v2v pods (`forklift.app=virt-v2v`) authenticate to vCenter and exchange metadata via the cluster's default network before transferring disks. Setting the default route to an IP address on the transfer network prevents connectivity to vCenter (`no route to host`).

## Solution

This webhook intercepts migration pod creation and removes the `default-route` field from the `k8s.v1.cni.cncf.io/networks` annotation while preserving the network attachment itself.

The webhook targets both migration pod types with separate `MutatingWebhookConfiguration` entries:

1. **virt-v2v webhook**: Pods with `forklift.app=virt-v2v` label
2. **CDI importer webhook**: Pods with `app=containerized-data-importer` label

Both parse the `k8s.v1.cni.cncf.io/networks` annotation, remove `default-route` fields, and return the modified pod specification.

**Example transformation:**

Before:
```json
[{"name":"mtv-transfer","namespace":"default","default-route":["192.168.0.1"]}]
```

After:
```json
[{"name":"mtv-transfer","namespace":"default"}]
```

This allows:
- Pods retain the transfer network as a secondary interface
- Default routing remains on the cluster's default network (for vCenter access)
- Direct layer 2 connectivity to ESXi hosts on the transfer network

**When to use this webhook:**

Use when all of the following are true:
- OVN-Kubernetes with localnet topology transfer network
- vCenter not reachable via the transfer network (special case: high-speed non-routable network directly connected to ESXi hosts)
- MTV transfer network feature enabled

Skip if vCenter is reachable via the transfer network gateway, you're not using MTV transfer networks, or your CNI accepts the legacy annotation.

## Deployment

### Prerequisites

- OpenShift 4.x cluster with MTV installed
- `oc` CLI configured and authenticated

### Deploy

The deployment uses a prebuilt image published to `ghcr.io/grandeit/gateway-yeeter:latest` via GitHub Actions on every push to `main`.

```bash
oc apply -k deploy/
```

Verify deployment:

```bash
oc get pods -n openshift-mtv -l app=gateway-yeeter
oc logs -n openshift-mtv -l app=gateway-yeeter -f
```

### High Availability

The deployment includes:
- **2 replicas** for redundancy
- **Pod anti-affinity** ensuring pods run on different nodes
- **PodDisruptionBudget** maintaining at least 1 pod during voluntary disruptions
- **Automatic TLS certificate management** via OpenShift's service-ca-operator

## Troubleshooting

Check the webhook logs for "YEETING" messages when migration pods are created:

```bash
oc logs -n openshift-mtv -l app=gateway-yeeter -f
```

Verify the webhook modified the pod's network annotation:

```bash
oc get pod <migration-pod> -n <namespace> -o jsonpath='{.metadata.annotations.k8s\.v1\.cni\.cncf\.io/networks}' | jq
```

The `default-route` field should be absent from the network configuration.

## Uninstall

```bash
oc delete -k deploy/
```

## Technical Details

### How Forklift Discovers and Sets the Transfer Network

Forklift's [`guessTransferNetworkDefaultRoute()`](https://github.com/kubev2v/forklift/blob/main/pkg/controller/plan/kubevirt.go#L2763) function discovers the gateway in priority order:

1. Checks the `forklift.konveyor.io/route` annotation on the NetworkAttachmentDefinition
2. Parses the NAD's `spec.config` JSON and searches the `ipam.routes` array for a default route (`0.0.0.0/0` or `::/0`), extracting the gateway IP

If a gateway is found, [`setTransferNetwork()`](https://github.com/kubev2v/forklift/blob/main/pkg/controller/plan/kubevirt.go#L2804) creates a `NetworkSelectionElement` with `GatewayRequest` set to the discovered IP and marshals it to the `k8s.v1.cni.cncf.io/networks` annotation. If no gateway is found, it falls back to setting `v1.multus-cni.io/default-network` with the NAD's namespaced name.

The code includes an _interesting_ comment: [`// FIXME: the codepath using the multus annotation should be phased out.`](https://github.com/kubev2v/forklift/blob/main/pkg/controller/plan/kubevirt.go#L2803)

### Why Both Annotation Paths Fail

The transfer network workflow creates a fundamental conflict:

| Gateway Discovered | Annotation Used | OVN Localnet Behavior | vCenter Reachability |
|-------------------|-----------------|----------------------|---------------------|
| Yes | `k8s.v1.cni.cncf.io/networks` with `default-route` | Network attaches | **Broken** - default route overrides cluster routing |
| No | `v1.multus-cni.io/default-network` | **Fails** - annotation not supported | N/A - network never attaches |

The webhook solves this by keeping the modern annotation format but removing the `default-route` field, allowing OVN to attach the network without disrupting pod routing.

### Migration Pod Selection Logic

MTV chooses between CDI importer pods and virt-v2v pods based on [`Plan.ShouldUseV2vForTransfer()`](https://github.com/kubev2v/forklift/blob/main/pkg/apis/forklift/v1beta1/plan.go#L292). For vSphere sources, virt-v2v is used only when **all** of these conditions are true:

- Not a warm migration (warm needs CDI snapshot delta handling)
- Destination is local (`destination.IsHost()` - can't monitor progress on remote clusters)
- `MigrateSharedDisks: true` (virt-v2v transfers all disks; CDI needed for selective disk migration)
- `SkipGuestConversion: false` (raw copy mode requires CDI)
- `Type != MigrationOnlyConversion` (conversion-only expects CDI-populated disks)

For OVA sources, `ShouldUseV2vForTransfer()` short-circuits to `true`.

