# Routeflare

<p align="center"><img src="content/routeflare.png" alt="Routeflarepher" width="100" height="100"></p>

Do you use the Gateway API for Kubernetes? Do you use Cloudflare for a DNS provider? If your answer to both of these questions was "yes" then this may have some interest for you. I was tired of needing external-dns, DDNS client installations, overly complex configurations, and rigid tools that just didn't do exactly what I wanted them to do even with all their complexity.

**So what did I want?**

My most basic needs were that I wanted a tool that would just watch for annotations on my HTTPRoutes and manage (A/AAAA) records in Cloudflare for me. That sounds like external-dns, right? Well, more than that, I wanted to be able to specify annotations on an HTTPRoute to manage records which...

 - always pointed to my current public IP address (taking the place of a DDNS client.)
 - pointed to the LoadBalancer Service IP of the associated Gateway for the HTTPRoute.

Note: This tool assumes you use Gateways and HTTPRoutes from Gateway API. Other Gateway API resources may be added to this over time.

## Deployment

See the [Helm Chart's readme.](chart/routeflare/README.md)

## Usage

This tool watches HTTPRoutes in your cluster and manages records for them based on annotations configured on the HTTPRoute. The supported annotations are as follows:

 - `routeflare/content-mode` - Specifies the mode that Routeflare should use to determine the content for the associated DNS record(s). Can be `gateway-address` or `ddns`. See #content-modes for more details.
 - `routeflare/type` - OPTIONAL: Specifies the type of DNS record to manage for this route. Can be `A`, `AAAA`, or `A/AAAA`. Defaults to `A`.
 - `routeflare/ttl` - OPTIONAL: Specifies the record's TTL in seconds (example: `360`). Defaults to auto.
 - `routeflare/proxied` - OPTIONAL Specifies whether or not to use Cloudflare's proxy. Can be `true` or `false`. Defaults to `false`.

`routeflare/content-mode` is the only required annotation. If this annotation is unspecified, Routeflare will ignore the HTTPRoute.

### Content modes

The `routeflare/content-mode` annotation on HTTPRoutes supports the following values:

- `gateway-address` will use the IPs specified in the Gateway's `status.addresses` specified as a parent of the HTTPRoute, for your record(s). It will take the first IPv4 address specified in `status.addresses` if `routeflare/type` is set to `A`, the first IPv6 address if set to `AAAA`, or the first occurrence of both if set to `A/AAAA`.

- `ddns` will detect the current IP address your cluster egresses to the world from and use that in the content for your record(s). Will attempt to automatically detect your current IPv4 address if `routeflare/type` is set to `A`, IPv6 if set to `AAAA`, or both if set to `A/AAAA`. A background job will run to detect if your address has changed and reconcile that with your `ddns` HTTPRoutes.

### Example

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  annotations:
    routeflare/content-mode: gateway-address
    # routeflare/type: A
    # routeflare/ttl: 360
    # routeflare/proxied: "true"
  name: prometheus
  namespace: monitoring
spec:
  hostnames:
    - prometheus.example.com
  parentRefs:
    - group: gateway.networking.k8s.io
      kind: Gateway
      name: default-internal
      namespace: gateway-system
  rules:
    - backendRefs:
        - group: ''
          kind: Service
          name: prometheus
          port: 9090
          weight: 1
      matches:
        - path:
            type: PathPrefix
            value: /
```

## Limitations

This tool is early on in development. Do not use this for production web services, this is intended for a homelab. 

One identified limitation of Routeflare is if you perform the following steps in order: Start Routeflare in your cluster, create an HTTPRoute with relevant annotations so that it creates a DNS record, stop Routeflare, delete the HTTPRoute, and finally start Routeflare back up again, then Routeflare will lose track of that DNS record and leave the record dangling in Cloudflare. This is because Routeflare doesn't know which zones it manages records in at startup. In a future release, there may be an optional configuration that declares which zone IDs to check at start for records owned by this Routeflare instance that no longer exist as HTTPRoutes in the cluster. For now, it assumes that you only delete HTTPRoutes while it is running. The trade off of this, is that in Routeflare's current state, it does not require knowing your zone IDs in advance, as long as the Cloudflare API token has permission to edit records in the zones associated with your HTTPRoutes. This makes Routeflare incredibly simple to configure and run.

If you find another limitation of Routeflare, please open up an Issue!
