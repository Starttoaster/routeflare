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

## Limitations

This tool is early on in development. Do not use this for production web services, this is intended for a homelab. 

One identified limitation of Routeflare is if you perform the following steps in order: If you start Routeflare in your cluster, create an HTTPRoute with relevant annotations for this tool so that it creates a DNS record, stop Routeflare, delete the HTTPRoute, and finally start Routeflare back up again, then Routeflare will lose track of that DNS record and leave the record dangling in Cloudflare. This is because Routeflare is entirely stateless, and doesn't track ownership of records using any mechanism in Cloudflare. The problem of ownership state could be handled a few different ways: External-DNS uses TXT records to close this limitation, though ownership metadata could also just be stored alongside the record in its comment section, or just in a local sqlite database. The best solution is being brainstormed.

Another limitation is that Routeflare doesn't try to account for state drift. If you manually change a record in Cloudflare, Routeflare currently will not attempt to fix it until either: the next restart of the Routeflare container, or the next time your HTTPRoute is updated.

Additionally, in the future this tool may support Routeflare annotations on Services, or even Ingresses (though the Ingress spec is a bit of a mess with competing APIs.) But it currently does not, and only supports HTTPRoutes.

If you find another limitation of Routeflare, please open up an Issue!
