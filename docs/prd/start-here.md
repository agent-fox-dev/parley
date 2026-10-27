
Goal is it to port "the hub" (https://github.com/agent-fox-dev/hub) from a golang version to typescript version that can be deployed on Cloudflare.

The reference is API (see docs/prd/api.md) and CLI (docs/prd/cli.md) compatibility. The implementation details are up for re-thinking but the overall behaviour of the migrated service must not differ from the hub implementation.

A success-criteria is when the afc CLI from hub works against a Parley API backend.

## Goals

G1: API compatible port to Cloudflare
G2: minimal web UI: landing page, login, simple dashboard with workspace view
G3: abstraction of the Cloudflare API/Resources so that a deployment to eg Kubernetes is possible.

## Approach

- Research the Hub implementation
- Research the Cloudflare API and services
- Ceate a first PRD that is then transformed into specs
 
## Stack

- typescript
- cloudflare backend:
    - Workers & Pages
    - Durable Objects
    - Queues
    - R2
    - D1
- react & vite, hono

Treat these as suggestions. If better alternatives exist, let's discuss

## References

Read and analythe the following resources as reference and for context:

-
- Hub: https://github.com/agent-fox-dev/hub
- agentkit-go: https://github.com/agent-fox-dev/agentkit-go
- workerd: https://github.com/cloudflare/workerd

