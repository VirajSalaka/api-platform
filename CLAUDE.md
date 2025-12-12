# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

WSO2 API Platform is an AI-ready, GitOps-driven API platform for full lifecycle management across cloud, hybrid, and on-premises deployments. The platform follows these core principles:

- **Developer experience is king**: Optimized workflows and UX
- **Minimal footprint**: Keep components as small as possible
- **Component independence**: No hard dependencies between components
- **GitOps ready**: Configuration as code for both API configs and gateway configs
- **AI-Ready by design**: Servers are MCP enabled for AI agent integration
- **Policy-first architecture**: Everything beyond basic proxy features is a policy

## Common Build Commands

### Gateway Development

```bash
# Build all gateway components (controller, router, policy-engine, gateway-builder)
make build-gateway

# Test all gateway components
make test-gateway

# Test specific gateway components
cd gateway && make test-controller
cd gateway && make test-policy-engine

# Build individual gateway components
cd gateway && make build-controller
cd gateway && make build-policy-engine
cd gateway && make build-router
cd gateway && make build-gateway-builder

# Clean gateway build artifacts
make clean-gateway
```

### Gateway Controller

```bash
cd gateway/gateway-controller

# Generate API server code from OpenAPI spec
make generate

# Run tests
make test  # or: go test -v ./... -cover

# Build Docker image
make build VERSION=1.0.0

# Copy policy definitions from policy manifest
make copy-policy-definitions

# Generate listener certificates
make generate-listener-certs
```

### Policy Engine

```bash
cd gateway/policy-engine

# Build Docker image
make build

# Run tests
make test  # or: go test -v ./...

# Run locally with docker-compose
make run

# View logs
make logs

# Stop services
make stop

# Run go mod tidy across all modules
make tidy
```

### Portal Development

```bash
# Management Portal (React + Vite + TypeScript)
cd portals/management-portal
npm install
npm run dev      # Start dev server on port 5173
npm run build    # Build for production
npm run lint     # Run ESLint
npm run preview  # Preview production build

# Developer Portal (Node.js + Express + Handlebars)
cd portals/developer-portal
npm install
npm run dev      # Start dev server on port 3001
npm run build    # Build for production
```

### Platform API

```bash
cd platform-api/src
# Build commands are Docker-based - see Dockerfile
```

### Kubernetes Operator

```bash
cd kubernetes/gateway-operator

# Install CRDs
make install

# Generate code
make generate

# Run tests
make test

# Build and push operator image
make docker-build docker-push IMG=<registry>/gateway-operator:tag
```

### Local Development (All-in-One)

```bash
# Start entire platform locally
cd distribution/all-in-one
docker compose up

# Rebuild images when code changes
docker compose up --build

# Shutdown (preserve data)
docker compose down

# Shutdown and clear data
docker compose down -v
```

### Version Management

```bash
# Show all component versions
make version

# Set specific version
make version-set COMPONENT=gateway VERSION_ARG=1.2.0

# Bump versions
make version-bump-patch COMPONENT=gateway
make version-bump-minor COMPONENT=gateway
make version-bump-major COMPONENT=gateway
make version-bump-next-dev COMPONENT=gateway  # Adds -SNAPSHOT suffix

# Release gateway components
make release-gateway VERSION_ARG=1.0.0
```

## Architecture Overview

The platform consists of independent components that work together:

### Gateway Components (Envoy-based)

The gateway runs as **four separate processes** that work together:

1. **Gateway-Controller** (`gateway/gateway-controller/`)
   - **Purpose**: xDS control plane managing API configurations
   - **Tech**: Go 1.25.1, Gin, Envoy go-control-plane, SQLite/memory storage
   - **Ports**: 9090 (REST API), 18000 (router xDS), 18001 (policy xDS)
   - **Key packages**: `pkg/api`, `pkg/controlplane`, `pkg/models`, `pkg/storage`, `pkg/xds`, `pkg/policyxds`
   - **Storage**: SQLite by default, memory mode available via `GATEWAY_STORAGE_TYPE=memory`
   - **Configuration**: Environment variables with `GATEWAY_` prefix, see `gateway/gateway-controller/config/config.yaml`

2. **Router** (`gateway/router/`)
   - **Purpose**: Envoy Proxy-based data plane
   - **Tech**: Envoy Proxy 1.35.3
   - **Ports**: 8080 (HTTP), 8443 (HTTPS), 9901 (admin)
   - **Config**: Receives dynamic config from Gateway-Controller via xDS (port 18000)

3. **Policy Engine** (`gateway/policy-engine/`)
   - **Purpose**: External processor for request/response processing
   - **Tech**: Go 1.24.0, gRPC ext_proc, CEL (Common Expression Language)
   - **Ports**: 9001 (ext_proc), 9002 (admin)
   - **Important**: NO built-in policies - all policies compiled via Gateway-Builder
   - **Config**: Receives policy config from Gateway-Controller via xDS (port 18001)
   - **Architecture**: Uses Envoy ext_proc filter for request/response interception

4. **Gateway-Builder** (`gateway/gateway-builder/`)
   - **Purpose**: Build-time tooling for discovering and compiling custom policies
   - **Tech**: Go 1.24.0
   - **Used during**: Docker build of Policy Engine to compile policies

### Platform Components

- **Platform API** (`platform-api/`)
  - Backend service powering portals and automation
  - Go 1.24.7, Gin, JWT, WebSocket, SQLite, libopenapi
  - Port: 8443 (HTTPS)
  - Manages: Organizations, Projects, Gateways, API lifecycle
  - Real-time deployment events via WebSocket

- **Management Portal** (`portals/management-portal/`)
  - Central control plane UI
  - React 19, TypeScript, Vite, Material-UI
  - Port: 5173

- **Developer Portal** (`portals/developer-portal/`)
  - API discovery and consumption portal
  - Node.js 22, Express, Handlebars, PostgreSQL (Sequelize)
  - Port: 3001

- **STS (Security Token Service)** (`sts/`)
  - OAuth 2.0 / OIDC server
  - Built on Asgardeo Thunder
  - Ports: 8090 (OAuth server), 9091 (Gate App UI)

- **Kubernetes Operator** (`kubernetes/gateway-operator/`)
  - Native Kubernetes deployment and lifecycle management
  - Go 1.25.1, controller-runtime, Helm
  - CRDs: `GatewayConfiguration`, `ApiConfiguration`

- **CLI** (`cli/`)
  - Command-line interface for API Platform operations
  - Go 1.25.4, Cobra framework

### SDK (`sdk/`)

Shared libraries and interfaces:
- `sdk/gateway/policy/` - Policy interfaces for custom policies
- `sdk/gateway/policyengine/` - Policy engine configuration types
- Used by both Policy Engine and custom policy implementations

## Key Architectural Patterns

### xDS-Based Dynamic Configuration

The Gateway-Controller implements Envoy's xDS v3 protocol:
- **Router xDS Server** (port 18000): Provides routes, listeners, clusters to Router
- **Policy xDS Server** (port 18001): Provides policy chains and configurations to Policy Engine
- Zero-downtime configuration updates
- See `gateway/gateway-controller/pkg/xds/` for router xDS implementation
- See `gateway/gateway-controller/pkg/policyxds/` for policy xDS implementation

### Policy Architecture

**CRITICAL**: The gateway ships with ZERO built-in policies.

- Policy manifest: `gateway/policies/policy-manifest.yaml` defines which policies to include
- Default policies in `gateway/gateway-controller/default-policies/` (BasicAuth, JWT, ModifyHeaders, Respond)
- Custom policies are Go plugins compiled via Gateway-Builder during Docker build
- Policy definitions use YAML format with CEL expressions for conditional execution
- Policy versioning supported (e.g., `v1.0.0`) - multiple versions can coexist
- Sample policies in `gateway/sample-policies/` for reference

**Policy Development Flow**:
1. Write policy code implementing `sdk/gateway/policy` interfaces
2. Add policy metadata in `policy-definition.yaml`
3. List policy in `policy-manifest.yaml`
4. Gateway-Builder discovers and compiles during Policy Engine build
5. Policy Engine loads compiled policies at runtime

### API Configuration Format

YAML-based declarative configs (see `gateway/examples/`):

```yaml
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: API-Name
  version: v1.0
  context: /path
  upstreams: [...]
  operations: [...]
  policies: [...]
```

### Storage Strategy

- **Gateway-Controller**: SQLite for embedded persistence (file: `storage.db`), memory mode available
- **Platform API**: SQLite
- **Developer Portal**: PostgreSQL
- All components support environment-based configuration

### Multi-Tenancy

- Organization-based isolation (Platform API)
- Project-based API grouping
- Gateway token-based authentication for gateway registration

## Testing Infrastructure

### Gateway Tests

```bash
# Run all gateway tests
make test-gateway

# Controller tests (unit + integration)
cd gateway/gateway-controller
go test -v ./...
go test -v ./tests/integration/...

# Policy Engine tests
cd gateway/policy-engine
go test -v ./...

# Individual policy tests
cd gateway/policies/jwt-auth/v1.0.0
go test -v ./...
```

**Integration tests** are in `gateway/gateway-controller/tests/integration/`:
- Concurrency tests
- Persistence tests
- Storage layer tests
- Schema validation tests

### Portal Tests

```bash
# Management Portal (Jest + React Testing Library)
cd portals/management-portal
npm test

# Developer Portal
cd portals/developer-portal
npm test
```

### Operator Tests

```bash
cd kubernetes/gateway-operator
make test  # or: go test ./...
```

## Configuration Patterns

### Gateway-Controller Environment Variables

All prefixed with `GATEWAY_`:
- `GATEWAY_STORAGE_TYPE`: `sqlite` or `memory` (default: `sqlite`)
- `GATEWAY_LOGGING_LEVEL`: `debug`, `info`, `warn`, `error` (default: `info`)
- `GATEWAY_SERVER_PORT`: REST API port (default: `9090`)
- `GATEWAY_XDS_PORT`: Router xDS port (default: `18000`)
- `GATEWAY_POLICY_XDS_PORT`: Policy xDS port (default: `18001`)

See `gateway/gateway-controller/config/config.yaml` for full configuration options.

### Policy Engine Configuration

- Config format: YAML
- CEL expressions for policy conditions
- Policy chains define execution order
- Configuration examples in `gateway/policy-engine/configs/`

### Developer Portal Configuration

- `config.json`: Database, auth, features, identity provider settings
- `secret.json`: Sensitive credentials (not checked into git)

## CI/CD Pipeline

GitHub Actions workflows in `.github/workflows/`:

- **gateway-release.yml**: Runs `make test-gateway` before release, multi-arch builds (amd64, arm64), auto-version bumping
- **gateway-snapshot.yml**: Automated snapshot releases on commits
- **operator-release.yml**: Kubernetes operator releases
- **gateway-helm-release.yml**: Helm chart releases for gateway
- **operator-helm-release.yml**: Helm chart releases for operator

All Docker images published to `ghcr.io/wso2/api-platform/`.

## Important Notes

### Go Module Structure

The repository has multiple Go modules:
- `gateway/gateway-controller/go.mod`
- `gateway/policy-engine/go.mod`
- `gateway/gateway-builder/go.mod`
- `platform-api/src/go.mod`
- `kubernetes/gateway-operator/go.mod`
- `cli/src/go.mod`
- `sdk/go.mod`
- Each policy has its own `go.mod`

When running `go mod tidy`, run it in each module directory separately, or use `make tidy` in policy-engine which handles multiple modules.

### Docker Build Context

Gateway components use Docker `--build-context` for sharing code:
- SDK is shared via `--build-context sdk=../../sdk`
- Policy Engine build includes gateway-builder, policies, and SDK contexts

### Version Files

Component versions stored in separate `VERSION` files:
- `VERSION` - Root/platform version
- `gateway/VERSION` - Gateway version (controller, router, policy-engine, gateway-builder)
- `platform-api/VERSION` - Platform API version

### Code Generation

Gateway-Controller uses oapi-codegen for OpenAPI server generation:
```bash
cd gateway/gateway-controller
make generate  # Generates from api/openapi.yaml
```

Always run after modifying `api/openapi.yaml`.

### Deployment Models

1. **All-in-One**: Docker Compose for local dev (`distribution/all-in-one/docker-compose.yaml`)
2. **Kubernetes Native**: Operator + Helm charts for production (`kubernetes/helm/gateway-helm-chart/`)
3. **VM/Bare Metal**: Individual Docker containers

## Quick Start Reference

```bash
# 1. Start platform locally
cd distribution/all-in-one
docker compose up

# 2. Create default organization
curl -k --location 'https://localhost:8443/api/v1/organizations' \
  --header 'Content-Type: application/json' \
  --header 'Authorization: Bearer <shared-token>' \
  --data '{
    "id": "15b2ac94-6217-4f51-90d4-b2b3814b20b4",
    "handle": "acme",
    "name": "ACME Corporation",
    "region": "US"
  }'

# 3. Access portals
# Management Portal: http://localhost:5173
# Developer Portal: http://localhost:3001
# Platform API: https://localhost:8443
```
