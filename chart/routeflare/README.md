# Routeflare Helm Chart

A Helm chart for deploying Routeflare, a Kubernetes controller that watches HTTPRoute objects and manages Cloudflare DNS records.

## Prerequisites

- Kubernetes 1.19+
- Helm 3.0+
- Gateway API CRDs installed
- Cloudflare API token with DNS write permissions

### Cloudflare API Token

You will need to go into your Cloudflare account or profile settings and create a new API token for Routeflare. It needs `dns:Edit` permissions in each zone that you want Routeflare to manage records in. See this [screenshot](../../content/routeflare-token.png) for an example.

## Installation

### Basic Installation

The simplest way to install Routeflare is to set your Cloudflare API token directly in the Helm values. The secret will be created automatically as part of the Helm release:

Create a `values.yaml` file:

```yaml
cloudflare:
  apiToken:
    createSecret: true
    value: "your-cloudflare-api-token"
  # Optional: Custom record owner ID (defaults to "routeflare" if not set)
  # Records created/updated by routeflare will have this value stored in the comment field
  # recordOwnerID: "my-custom-owner"
```

Then install with:

```bash
helm upgrade --install --create-namespace \
--repo "https://starttoaster.github.io/routeflare" 
-n routeflare \
--values ./values.yaml \
routeflare \
routeflare
```

### Using an Existing Secret

For production deployments or when using external secret management systems (e.g., Sealed Secrets, External Secrets Operator), use an existing secret:

1. Create the secret first:

```bash
kubectl create secret generic routeflare-cloudflare-token \
  --from-literal=token='your-cloudflare-api-token' \
  -n routeflare
```

2. Install the chart referencing the existing secret:

```bash
helm upgrade --install --create-namespace \
  --repo "https://starttoaster.github.io/routeflare" 
  -n routeflare \
  --set cloudflare.apiToken.existingSecret=true \
  --set cloudflare.apiToken.secretName=routeflare-cloudflare-token \
  routeflare \
  routeflare
```
