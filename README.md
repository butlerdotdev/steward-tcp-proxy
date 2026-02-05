# steward-tcp-proxy

A lightweight TCP proxy for Steward tenant clusters that solves the "wrong endpoint" problem.

## The Problem

When running Kubernetes control planes in a management cluster (via Steward), 
the tenant cluster's `kubernetes` Service and EndpointSlice point to the external API 
server address. This breaks in-cluster clients that expect to reach the API server 
via the `kubernetes.default.svc` ClusterIP.

## The Solution

`steward-tcp-proxy` runs inside the tenant cluster and:

1. **Rewrites the EndpointSlice** - Updates the `kubernetes` EndpointSlice to point to itself
2. **Proxies traffic** - Forwards all connections to the real API server

This allows in-cluster clients to use the standard `kubernetes.default.svc:443` endpoint
while the proxy handles routing to the actual control plane.

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                      Tenant Cluster                              │
│                                                                  │
│  ┌──────────────┐                         ┌───────────────────┐ │
│  │   In-cluster │                         │  kubernetes       │ │
│  │   Client     │──────────────────┐      │  EndpointSlice    │ │
│  │   (kubectl)  │                  │      │  (managed)        │ │
│  └──────────────┘                  │      └─────────┬─────────┘ │
│                                    │                │           │
│                                    ▼                ▼           │
│                           ┌─────────────────────────────────┐   │
│                           │  steward-tcp-proxy              │   │
│                           │  Service (ClusterIP)            │   │
│                           │  ↓                              │   │
│                           │  tcp-proxy Pod                  │   │
│                           │  - Proxies TCP to upstream      │   │
│                           │  - Reconciles EndpointSlice     │   │
│                           └─────────────────┬───────────────┘   │
└─────────────────────────────────────────────┼───────────────────┘
                                              │
                                              ▼
                              ┌───────────────────────────────┐
                              │  Management Cluster           │
                              │  kube-apiserver (real)        │
                              └───────────────────────────────┘
```

## Usage

### Command Line Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--listen-addr` | `:6443` | Address to listen on |
| `--upstream-addr` | (required) | Real API server address (host:port) |
| `--service-name` | `steward-tcp-proxy` | Name of the tcp-proxy Service |
| `--service-port` | `6443` | Port of the tcp-proxy Service |
| `--reconcile-interval` | `30s` | How often to reconcile EndpointSlice |

The upstream address can also be set via the `UPSTREAM_ADDR` environment variable.

### Kubernetes Deployment

The proxy is typically deployed by Steward's tcp-proxy addon. When you enable tcp-proxy 
on a TenantControlPlane:

```yaml
apiVersion: steward.butlerlabs.dev/v1alpha1
kind: TenantControlPlane
metadata:
  name: my-tenant
spec:
  addons:
    tcpProxy: {}
```

Steward automatically:
1. Adds `--endpoint-reconciler-type=none` to the API server (disables built-in reconciler)
2. Deploys tcp-proxy with the correct upstream address
3. Creates the Service, ServiceAccount, and RBAC

## Building

```bash
# Build binary
make build

# Build Docker image
make docker-build

# Build and push multi-arch
make docker-buildx
```

## Requirements

The tcp-proxy needs RBAC permissions to:
- Get Services in `kube-system` namespace
- Get/Update/Create EndpointSlices in `default` namespace

These are created by Steward's tcp-proxy addon.

## License

Apache License 2.0 - see [LICENSE](LICENSE)
