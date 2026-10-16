# Custom tabs

A custom tab puts a web page next to your server's console and files. There are
two kinds, and they need very different things from you.

| | What it does | What you need |
|---|---|---|
| **Direct** | Points the browser at a URL you give it | Nothing. The URL has to be reachable from your users' browsers. |
| **Proxied** | Shows a port **inside** the server container, through Dylaris | A DNS wildcard and a certificate. This page. |

Direct tabs work out of the box. Everything below is about proxied tabs, which
are what you want for a map, a dashboard or any web UI that is running inside
the container and is not exposed to the internet.

---

## Why proxied tabs need their own hostname

Each proxied tab is served at the **root of its own hostname**:

```
https://<random-label>.tabs.example.com/
```

Not under a path like `/tabs/17/`. That is not a style choice, it is the only
arrangement that works:

**Applications ask for their files from the root.** BlueMap requests
`/js/app.<hash>.js` and `/settings.json`; Dynmap wants `config.js`, `css/` and
`up.php`. Those paths start at the root of whatever host they are on. Under a
path prefix they miss it entirely, and no amount of rewriting from outside fixes
it - both projects tell their own users to use a subdomain for the same reason.

**A separate hostname is a separate origin.** The page inside a tab is software
you or your customer chose, not software Dylaris wrote. On the panel's own
hostname its JavaScript would sit inside the panel and act as whoever is signed
in. On its own hostname it reaches neither the panel's pages nor its session,
which is a cookie scoped to the panel's hostname alone.

---

## What you need

1. A **wildcard DNS record** for the suffix you pick, pointing at the same place
   your panel points.
2. A **certificate covering both** the suffix and its wildcard. A wildcard
   certificate does not cover its own parent: `*.tabs.example.com` is not valid
   for `tabs.example.com`, so both names go on one certificate.
3. Wildcard certificates can only be issued over the **DNS-01** challenge. HTTP-01
   cannot issue them, so you need a DNS provider your ACME client can talk to.
4. `TAB_PROXY_HOST_SUFFIX` on Core.

If you cannot meet 1-3, leave `TAB_PROXY_HOST_SUFFIX` empty. Proxied tabs are
then unavailable and say so; direct tabs keep working.

---

## Step 1: DNS

Pick a suffix. It has to be a domain suffix, not a single label:

```
tabs.example.com      A   <same address as your panel>
*.tabs.example.com    A   <same address as your panel>
```

Create **both** records explicitly, even if a catch-all higher up the zone would
answer them. A catch-all hides a missing record: names resolve, traffic arrives
somewhere, and nothing reports an error.

> **Behind Cloudflare's proxy?** A free Universal SSL certificate covers
> `example.com` and `*.example.com` - one level. `*.tabs.example.com` is a second
> level and is **not** covered, so every tab fails TLS at Cloudflare's edge before
> reaching you. Either set both records to **DNS only** (grey cloud) so your own
> certificate is the one browsers see, or buy Advanced Certificate Manager.

## Step 2: Certificate

One certificate, two names. With Traefik and a DNS-01 resolver, on the router
that serves the tabs:

```yaml
- "traefik.http.routers.dylaris-tabs.tls.certresolver=<your-dns01-resolver>"
- "traefik.http.routers.dylaris-tabs.tls.domains[0].main=tabs.example.com"
- "traefik.http.routers.dylaris-tabs.tls.domains[0].sans=*.tabs.example.com"
```

With certbot:

```bash
certbot certonly --dns-<provider> \
  -d tabs.example.com -d '*.tabs.example.com'
```

Check that your resolver really uses DNS-01. The name of a resolver says nothing
about its challenge type, and an HTTP-01 resolver will simply fail to issue the
wildcard.

## Step 3: Reverse proxy

Two rules. The bare suffix serves the **panel** (it renders the share page with
the header and the link back). Anything one label below it serves **Core** (the
tab content itself).

They must be different hosts. If the share page and the tab content shared an
origin, the framed page could reach into the page framing it.

### Traefik

On the Core service (it serves the panel bundle):

```yaml
- "traefik.http.routers.dylaris-share.rule=Host(`tabs.example.com`)"
- "traefik.http.routers.dylaris-share.entrypoints=websecure"
- "traefik.http.routers.dylaris-share.tls=true"
- "traefik.http.routers.dylaris-share.tls.certresolver=<your-dns01-resolver>"
- "traefik.http.routers.dylaris-share.service=<your-core-service>"
```

On the Core service (no new service needed - the tabs are served on Core's
normal port):

```yaml
- "traefik.http.routers.dylaris-tabs.rule=Host(`*.tabs.example.com`)"
- "traefik.http.routers.dylaris-tabs.entrypoints=websecure"
- "traefik.http.routers.dylaris-tabs.tls=true"
- "traefik.http.routers.dylaris-tabs.tls.certresolver=<your-dns01-resolver>"
- "traefik.http.routers.dylaris-tabs.tls.domains[0].main=tabs.example.com"
- "traefik.http.routers.dylaris-tabs.tls.domains[0].sans=*.tabs.example.com"
- "traefik.http.routers.dylaris-tabs.service=<your-core-service>"
```

`Host()` with a leading wildcard matches exactly one label, which is what keeps
`tabs.example.com` itself out of this rule. On older Traefik versions that do not
support it, use the regular expression instead:

```yaml
- "traefik.http.routers.dylaris-tabs.rule=HostRegexp(`^[a-z0-9]+\\.tabs\\.example\\.com$`)"
```

### nginx

```nginx
server {
    listen 443 ssl;
    server_name tabs.example.com;
    ssl_certificate     /etc/letsencrypt/live/tabs.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/tabs.example.com/privkey.pem;
    location / { proxy_pass http://core:25500; }   # the wrapper IS the panel bundle, served by Core
}

server {
    listen 443 ssl;
    server_name ~^[a-z0-9]+\.tabs\.example\.com$;
    ssl_certificate     /etc/letsencrypt/live/tabs.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/tabs.example.com/privkey.pem;

    location / {
        proxy_pass http://core:25500;
        proxy_set_header Host $host;          # REQUIRED: the host is the routing key
        proxy_set_header X-Forwarded-Proto $scheme;

        # Tabs may hold a WebSocket (live maps do).
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_read_timeout 3600s;
    }
}
```

`proxy_set_header Host $host` is not optional. Core decides which tab a request
belongs to from the hostname, so a proxy that rewrites it makes every tab a 404.

### Caddy

```
tabs.example.com {
    reverse_proxy core:25500   # the wrapper IS the panel bundle, served by Core
}

*.tabs.example.com {
    reverse_proxy core:25500
    tls {
        dns <your-provider> {env.YOUR_API_TOKEN}
    }
}
```

## Step 4: Core

```yaml
TAB_PROXY_HOST_SUFFIX: "tabs.example.com"
```

Set it in `.env` (or your stack's environment): both shipped compose files
already pass `TAB_PROXY_HOST_SUFFIX` through to Core. The
value is cleaned up for you: a scheme, a trailing slash, a port or a leading dot
are all stripped. A single label with no dot is refused, with a line in the log.

Restart Core.

## Step 5: Turn it on in the panel

Both switches default to off.

1. **Settings -> Features -> Custom tabs -> Tab proxy.** The note underneath tells
   you whether Core has a proxy host; if it still says the feature is
   unavailable, `TAB_PROXY_HOST_SUFFIX` did not arrive.
2. **Allow public share links** - only if you want links that work without
   signing in.
3. **Settings -> Modules -> Custom Tabs** adds the navigation entry that shows
   every tab you can reach, full width.

---

## Creating a tab

**Server -> Config -> Tabs -> New.**

- **Mode**: proxied
- **Target port**: the port inside the container, e.g. `8123` for BlueMap or
  Dynmap, `8100` for squaremap
- **Target path**: usually `/`
- **Surface**: `tab` shows it in the panel, `page` gives it a standalone share
  link, `both` does both
- **Visibility**: `private` requires a sign-in with access to that server;
  `public` is anyone with the link, and only works while public share links are
  enabled
- **Sub-server**: which world this tab belongs to, or every one of them

The tab gets its hostname when you create it. Share links are separate from that
hostname, so rotating a link does not change where the content lives.

### Sub-servers

A proxied tab points at a port **inside the container**, and the container runs
whichever sub-server is started. An unpinned tab therefore follows whatever is
running - right for a plugin every world has, wrong for a map that only one of
them serves.

Pin the tab to a sub-server and it is refused while a different one is running,
with a page saying so, instead of showing another world's map under the name of
yours. Leave it on **Every sub-server** and it behaves as before.

### Share links

The **Generate** button mints an unguessable link. **Choose my own** takes a
name you pick instead: lowercase letters, digits and single hyphens, 4 to 40
characters, and it has to be free across the whole instance.

    https://tabs.example.com/c/max-survival-map

A chosen name is guessable, which is the trade for a readable one. That only
matters for a `public` link, and a public link is meant to be handed out
anyway. A `private` link is gated by your sign-in, not by the name, so a
readable one gives nothing away.

### Limits

An admin sets two, both **per user**, under Settings -> Features -> Custom tabs:

| | Default |
|---|---|
| Proxied tabs per server | 3 |
| Proxied tabs in total | 10 |
| Share links per user | 20 |

The total is the one that actually bounds an account - the per-server figure
alone would let somebody with twenty servers hold twenty times it. Direct tabs
count against neither: they cost the platform nothing to serve.

---

## Verifying

In this order. Each step tells you which one failed.

```bash
# 1. DNS answers for both the suffix and a random label under it
dig +short tabs.example.com aaaaaaaaaaaaaaaaaaaa.tabs.example.com

# 2. The certificate carries BOTH names
echo | openssl s_client -connect tabs.example.com:443 \
  -servername aaaaaaaaaaaaaaaaaaaa.tabs.example.com 2>/dev/null \
  | openssl x509 -noout -text | grep -A1 "Subject Alternative Name"

# 3. The suffix serves the panel
curl -sI https://tabs.example.com/ | head -1

# 4. An unknown label reaches CORE and answers 404 as plain text,
#    not the panel's HTML error page
curl -sI https://aaaaaaaaaaaaaaaaaaaa.tabs.example.com/ | head -3

# 5. The API is NOT reachable there. This one matters:
#    a tab host must serve tabs and nothing else.
curl -s -o /dev/null -w '%{http_code}\n' \
  https://aaaaaaaaaaaaaaaaaaaa.tabs.example.com/api/system/core-info
```

Step 5 must print `404`. Anything else means the hostname rule is not matching
and a tab's page would share an origin with your API.

---

## Troubleshooting

**The tab is blank, or the browser console shows 404s for `/js/...`.**
The content is not being served at the root. Check that your proxy passes the
original `Host` header and does not strip a path.

**"This page is private" although I am signed in.**
Expected, and it is not a misconfiguration. Your session is a cookie scoped to
the panel's hostname, so nothing on the share hostname can see it or prove who
you are. A PUBLIC link needs no proof and works for anyone; a private tab has to
be opened from the panel, which is the one place that can authorize it. Opening
it there authorizes that tab's hostname for a few minutes, so a private share
link is a shortcut for someone who has just used the panel, never a way in.

**The application inside the tab wants me to log in and never stays logged in.**
Cookies are not passed through a proxied tab, in either direction. That is
deliberate: cookies are shared across a whole domain rather than per hostname, so
a page inside a tab could otherwise set cookies your panel would receive.
Applications that keep their session in a cookie (Plan with authentication
enabled, Dynmap with `login-enabled`, Grafana) cannot be used this way. Use a
direct tab with a real public URL for those.

**The tab refuses to display and the console mentions `X-Frame-Options`.**
The application forbids being framed. That is its own setting, not ours - Grafana
for instance needs `allow_embedding = true`.

**Certificate errors on tab hostnames only.**
Almost always the wildcard depth: a certificate for `*.example.com` does not
cover `abc.tabs.example.com`, and neither does one for `*.tabs.example.com` cover
`tabs.example.com`. Check the SANs with step 2 above.
