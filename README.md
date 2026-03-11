## Developer Tutorial: Add a new tunneling protocol

This branch introduces a generic external-networking scaffold to simplify the integration of additional tunneling technologies.

The design separates:

* **Protocol-agnostic logic**: gateway process bootstrap, startup orchestration, shared connection health checks, and status updates.
* **Protocol-specific logic**: tunnel setup/maintenance, protocol CRDs, protocol controllers, protocol runtime behavior.

### Why this exists

Different tunneling protocols require different runtime, reconciliation logic and security parameters, while parts of the system are always the same.
This scaffold lets developers reuse common pieces and focus only on protocol-specific implementation.

### What you need to implement

To add a new protocol (`<protocol>`), implement the following:

1. New protocol CRDs:
  * `<protocol>GatewayClient`
  * `<protocol>GatewayClientTemplate`
  * `<protocol>GatewayServer`
  * `<protocol>GatewayServerTemplate`
2. Protocol controllers:
  * Reconcile protocol CRs.
  * Materialize protocol resources (e.g., deployment/service/secrets/config).
  * Keep protocol resources aligned with the desired state.
3. Gateway protocol runtime:
  * Configure and run the actual tunnel process in the gateway pod.
  * Integrate with shared gateway startup flow.
4. Connection bootstrap:
  * Ensure a `Connection` resource exists when the protocol is ready to attempt connectivity.
  * Set initial status to `Connecting`.

### Reusable protocol-agnostic components already provided

* `pkg/gateway/generic/container.go`: common manager bootstrap for protocol containers.
* `pkg/gateway/connection/connections_controller.go`: protocol-agnostic connection status reconciler.
* `pkg/gateway/connection/conncheck`: active connectivity checks and latency sampling.

### Connection lifecycle in this architecture

1. Protocol controller/runtime prepares tunnel prerequisites.
2. Protocol logic creates or updates the `Connection` resource and sets status to `Connecting`.
3. Protocol-agnostic conncheck verifies actual reachability.
4. `Connection` status is updated to `Connected` or `ConnectionError` (with latency data when enabled).

### Development checklist

* Define and register protocol CRDs.
* Implement protocol controllers and ownership/labeling conventions.
* Implement protocol container/runtime startup and reconciliation hooks.
* Ensure `Connection` bootstrap occurs in protocol code path.
* Validate behavior with leader election enabled and disabled.
* Validate reconfiguration/recovery on restarts and endpoint changes.

## Implementation walkthrough

This section describes the practical steps to implement a new protocol in this branch.

### Change map (quick navigation)

| Area | Files to touch | Mandatory | Purpose |
|---|---|---|---|
| Protocol CRDs | `apis/networking/v1beta1/<protocol>gatewayclient_types.go`<br>`apis/networking/v1beta1/<protocol>gatewayclienttemplate_types.go`<br>`apis/networking/v1beta1/<protocol>gatewayserver_types.go`<br>`apis/networking/v1beta1/<protocol>gatewayservertemplate_types.go` | Yes | Define protocol API contracts (`Spec`/`Status`) and schema validation. |
| CRD/RBAC generation | `Makefile` targets: `manifests`, `rbacs`, `generate` | Yes | Regenerate CRDs and RBAC after API/controller changes. |
| Generic external-network wiring | `pkg/liqo-controller-manager/networking/external-network/generic/genericgatewayclient_controller.go`<br>`pkg/liqo-controller-manager/networking/external-network/generic/genericgatewayserver_controller.go` | Yes | Bind protocol-specific wrappers and callbacks to shared reconciliation flow. |
| Protocol external-network package | `pkg/liqo-controller-manager/networking/external-network/<protocol>/...` | Yes | Implement protocol adapters: options, utilities, predicates, enqueuers, custom watches. |
| Controller-manager startup | `cmd/liqo-controller-manager/modules/networking.go` | Yes | Instantiate protocol reconcilers and register them with manager (`SetupWithManager`). |
| Protocol runtime entrypoint | `cmd/gateway/<protocol>/main.go` | Yes | Start protocol runtime process and integrate startup/orchestration behavior. |
| Shared container scaffold | `pkg/gateway/generic/container.go` | Recommended | Reuse manager bootstrap and startup synchronization pattern. |
| Protocol tunnel runtime | `pkg/gateway/tunnel/<protocol>/...` | Yes | Implement tunnel session/device setup and protocol runtime lifecycle. |
| Connection bootstrap | `pkg/gateway/tunnel/<protocol>/k8s.go` (or equivalent) | Yes | Create/update `Connection` and initialize state (`Connecting`). |
| Connection status checks | `pkg/gateway/connection/connections_controller.go`<br>`pkg/gateway/connection/conncheck/...` | Usually no changes | Shared, protocol-agnostic status and latency updates. |
| Leader-election/container sync | `pkg/gateway/options.go`<br>`pkg/gateway/flags.go`<br>`pkg/gateway/concurrent/gateway.go`<br>`pkg/gateway/concurrent/guest.go`<br>`cmd/gateway/main.go` | Depends | Keep container names/options consistent for IPC startup coordination. |
| Deployment wiring | `deployments/liqo/...` (gateway manifests/charts) | Yes | Ensure new protocol container image, args, env, and container names are declared. |

### 1. Create and generate protocol CRDs

You need four CRDs for each protocol:

* `<protocol>GatewayClient`
* `<protocol>GatewayClientTemplate`
* `<protocol>GatewayServer`
* `<protocol>GatewayServerTemplate`

Files to update:

* `apis/networking/v1beta1/<protocol>gatewayclient_types.go`
* `apis/networking/v1beta1/<protocol>gatewayclienttemplate_types.go`
* `apis/networking/v1beta1/<protocol>gatewayserver_types.go`
* `apis/networking/v1beta1/<protocol>gatewayservertemplate_types.go`
* `apis/networking/v1beta1/groupversion_info.go` (if you need to register additional types manually)

Responsibility of this module:

* Define protocol API surface and validation schema.
* Define `Spec` and `Status` fields used by protocol and generic controllers.

After adding API types and kubebuilder markers, regenerate manifests with Makefile targets already available in this repository:

```bash
make manifests
make rbacs
make generate
```

Notes:

* `make manifests` runs `controller-gen` for CRDs.
* `make rbacs` regenerates RBAC manifests from kubebuilder RBAC markers.
* `make generate` runs the full generation pipeline (`generate-groups`, `rbacs`, `manifests`, `fmt`).

### 2. Implement generic external-network controllers for your protocol

The `GenericGatewayServer` and `GenericGatewayClient` controllers are the integration point for protocol-specific resources.

Conceptually:

* The generic controller flow handles shared reconciliation mechanics.
* Protocol callbacks and protocol CR wrappers provide protocol-specific behavior.

Files to update:

* `pkg/liqo-controller-manager/networking/external-network/generic/genericgatewayclient_controller.go`
* `pkg/liqo-controller-manager/networking/external-network/generic/genericgatewayserver_controller.go`
* `pkg/liqo-controller-manager/networking/external-network/<protocol>/` (new package with protocol wiring and helpers)

Reference implementation:

* `pkg/liqo-controller-manager/networking/external-network/wireguard/wggatewayclient_controller.go`
* `pkg/liqo-controller-manager/networking/external-network/wireguard/wggatewayserver_controller.go`

Responsibility of this module:

* Translate desired state (`GatewayClient` / `GatewayServer`) into protocol-specific resources.
* Connect protocol behavior to the generic reconciliation framework through interfaces and callbacks.

#### 2.1 Implement the protocol interface wrappers

Implement the protocol interfaces (`GenericGwServer` and `GenericGwClient`) using one-liner getters/setters mapped to your CR structure.

Typical methods expose:

* deployment/service templates
* metrics settings
* secret references
* endpoint status fields
* internal endpoint status fields

This is similar to the WireGuard pattern where wrapper methods just map generic controller expectations to protocol CR fields.

Example shape:

```go
func (p *ProtocolGatewayServer) GetDeploymentTemplate() *networkingv1beta1.DeploymentTemplate {
  return &p.Spec.Deployment
}

func (p *ProtocolGatewayServer) GetServiceTemplate() *networkingv1beta1.ServiceTemplate {
  return &p.Spec.Service
}

func (p *ProtocolGatewayServer) GetSecretRef() corev1.LocalObjectReference {
  return p.Spec.SecretRef
}

func (p *ProtocolGatewayServer) SetEndpointStatus(endpoint *networkingv1beta1.EndpointStatus) {
  p.Status.Endpoint = endpoint
}
```

#### 2.2 Implement protocol options structs (`OptionsServer` and `OptionsClient`)

These structs are the most important protocol extension point. They usually contain callback functions that perform protocol-specific actions, such as:

* ensuring key/certificate secrets
* validating protocol prerequisites
* reconciling protocol side resources
* updating protocol endpoint/runtime configuration

A practical pattern is to place callback implementations in protocol utility files and only wire functions in the controller setup code, to keep reconcilers small and readable.

Files to update:

* `pkg/liqo-controller-manager/networking/external-network/<protocol>/` (new files, for example `options.go` and `utils.go`)
* `pkg/liqo-controller-manager/networking/external-network/generic/genericgatewayclient_controller.go` (options wiring)
* `pkg/liqo-controller-manager/networking/external-network/generic/genericgatewayserver_controller.go` (options wiring)

#### 2.3 Extend builder setup when needed

When your protocol needs additional watches, add them through a protocol-specific custom builder setup hook.

Examples of extra watches:

* Pods (runtime readiness changes)
* Secrets (keys/certs/config)
* ClusterRoleBinding or other permissions-related resources

Files to update:

* `pkg/liqo-controller-manager/networking/external-network/<protocol>/...` (enqueuers and predicates)
* `pkg/liqo-controller-manager/networking/external-network/generic/genericgatewayclient_controller.go` (custom builder hook wiring)
* `pkg/liqo-controller-manager/networking/external-network/generic/genericgatewayserver_controller.go` (custom builder hook wiring)

Example shape:

```go
CustomBuilderSetup: func(b *builder.Builder) *builder.Builder {
  return b.
    Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(podEnqueuer)).
    Watches(&rbacv1.ClusterRoleBinding{}, handler.EnqueueRequestsFromMapFunc(clusterRoleBindingEnqueuer)).
    Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(secretEnqueuer),
      builder.WithPredicates(filterProtocolSecretsPredicate()))
}
```

### 3. Start protocol controllers from controller-manager

Defining protocol controllers is not enough: they must be instantiated and registered in the controller manager startup path.

For each protocol controller:

1. Create the reconciler instance.
2. Call `SetupWithManager(...)`.
3. Ensure required options/callbacks are correctly injected.

Files to update:

* `cmd/liqo-controller-manager/modules/networking.go`

Reference implementation:

* `cmd/liqo-controller-manager/modules/networking.go` (WireGuard controller startup wiring)

Responsibility of this module:

* Register protocol controllers in the main manager lifecycle so they actually run.

### 4. RBAC for new protocol controllers

Add dedicated ClusterRole/Role markers for the new protocol controllers.

In most cases, protocol controllers will require permissions very similar to existing WireGuard controllers. Reuse the same permission model, adapting only resource names and controller identifiers for clarity.

Files to update:

* protocol controller files where kubebuilder RBAC markers are declared (for example in `pkg/liqo-controller-manager/networking/external-network/<protocol>/...`)
* `Makefile` (`rbacs` target already available; run generation)
* generated manifests under `deployments/liqo/files/` (do not edit manually, regenerate)

Responsibility of this module:

* Ensure each controller has least-privilege access to the resources it watches and reconciles.

### 5. Implement the protocol container entrypoint

The protocol container is responsible for runtime tunnel behavior in the gateway pod.

At minimum it should:

1. Bootstrap the manager (health probes, cache, shared setup).
2. Integrate startup synchronization with gateway leader election logic when enabled.
3. Start protocol runtime logic (for example, run protocol binary, configure netlink interfaces, configure sessions/peers).
4. Register and run the `Connection` status controller if required by the runtime design.

Files to update:

* `cmd/gateway/<protocol>/main.go` (new protocol binary entrypoint)
* `pkg/gateway/generic/container.go` (shared startup scaffold, reused by protocol containers)
* `pkg/gateway/tunnel/<protocol>/...` (protocol runtime logic)

Reference implementation:

* `cmd/gateway/wireguard/main.go`
* `pkg/gateway/tunnel/wireguard/`

Responsibility of this module:

* Run and maintain protocol runtime in the gateway pod process space.

### 6. Connection ownership model

Use this model to avoid race conditions and ambiguity:

1. Protocol logic creates or updates the `Connection` resource when tunnel prerequisites are ready.
2. Protocol logic sets initial status to `Connecting`.
3. The protocol-agnostic connection checker updates runtime status (`Connected` or `ConnectionError`) and latency.

In other words:

* protocol code owns `Connection` bootstrap and protocol-specific readiness threshold
* protocol-agnostic code owns ongoing health observation and status transitions

Files to update:

* `pkg/gateway/tunnel/<protocol>/k8s.go` (or equivalent) to create/update `Connection`
* `pkg/gateway/connection/connections_controller.go` (shared status reconciler, usually reused as-is)
* `pkg/gateway/connection/conncheck/` (shared connectivity checks, usually reused as-is)

Reference implementation:

* `pkg/gateway/tunnel/wireguard/k8s.go` (`EnsureConnection`)
* `pkg/gateway/tunnel/wireguard/publickeys_controller.go` (where `EnsureConnection` is invoked)

Responsibility of this module:

* Keep ownership boundaries clear between protocol bootstrap and generic status management.

### 7. Container naming and leader election caveat

When startup synchronization is enabled, container names must be consistent across:

* gateway deployment container definitions
* concurrent containers configuration passed to gateway options
* protocol container runtime options

If names are inconsistent, IPC-based startup orchestration can block one or more containers from starting correctly.

Files to update/check:

* `pkg/gateway/options.go` (`ConcurrentContainersNames`, leader-election options)
* `pkg/gateway/flags.go` (CLI flags including `--concurrent-containers-names`)
* `pkg/gateway/concurrent/gateway.go` (orchestrator side)
* `pkg/gateway/concurrent/guest.go` (protocol container side)
* `cmd/gateway/main.go` (gateway orchestrator container)
* `cmd/gateway/<protocol>/main.go` (protocol container startup)
* gateway Deployment templates/manifests under `deployments/liqo/` where container names are defined

Responsibility of this module:

* Coordinate startup order and synchronization between gateway containers.

## Suggested validation flow for a new protocol

1. Create protocol CRDs and regenerate manifests.
2. Deploy CRDs and controller changes.
3. Verify protocol controllers reconcile `GatewayServer` and `GatewayClient` resources.
4. Verify protocol runtime container starts and configures the tunnel.
5. Verify `Connection` is created and transitions from `Connecting` to `Connected`.
6. Test failure scenarios and confirm status moves to `ConnectionError` and recovers.

## Scope of this tutorial

This guide documents the branch scaffolding and integration model.
It does not define protocol-specific cryptography/session semantics; those must be implemented by each protocol backend.