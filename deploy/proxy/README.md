# The reference reverse proxy

Hexagon has no TLS of its own and should not grow any: it binds loopback, and
everything about being published — certificates, the redirect from plaintext,
HTTP/2, connection limits — belongs to something that already does it well.
This is the configuration known to work, in the same spirit as
[the reference session image](../images/base/Dockerfile): an example, not a
deployment.

Hexagon itself is **not** in this compose file, and should not be. It drives the
Docker socket, runs session containers as the host user and bind mounts host
paths into them; inside a container those paths would name the container's
filesystem while Docker resolved them against the host's, so every session would
mount the wrong directory. Hexagon runs on the host. Only the proxy is
containerised.

## What to change

1. **`server_name`** in [nginx.conf](nginx.conf), in both server blocks.
2. **The certificates**, as `certs/fullchain.pem` and `certs/privkey.pem`.
   `certs/` is gitignored.
3. **`HEXAGON_PUBLIC_URL`**, to exactly `https://<server_name>`, and the OAuth
   App's callback URL to `https://<server_name>/api/auth/callback`. GitHub
   compares that one verbatim.

Leave `HEXAGON_ADDR` at `127.0.0.1:8080`. The loopback bind is the last line of
defence if this proxy is ever misconfigured, and Hexagon refuses to start on an
address the network can reach unless its public URL is https anyway.

Then:

```sh
docker compose up -d
```

## A certificate for testing

```sh
openssl req -x509 -newkey rsa:2048 -nodes -days 365 \
  -keyout certs/privkey.pem -out certs/fullchain.pem \
  -subj "/CN=hexagon.example" -addext "subjectAltName=DNS:hexagon.example"
```

The browser will not trust it. Add the name to `/etc/hosts` pointing at
`127.0.0.1`, or test with `curl --resolve hexagon.example:443:127.0.0.1 -k`.

Put it in `certs/`, which is gitignored:

```sh
mkdir -p certs
```

## Checking it works

```sh
curl -sI http://hexagon.example/                 # 301 to https
curl -s  https://hexagon.example/api/health      # {"status":"ok"}
curl -sI https://hexagon.example/ | grep -i strict-transport   # Hexagon's HSTS
```

Then in a browser: sign in, open a session terminal, leave it idle for five
minutes and type into it, and open the **VS Code** button. The terminal and the
editor are both WebSockets, which is what the `Upgrade` headers and the one-hour
read timeout in [nginx.conf](nginx.conf) are for.

Checking the WebSocket path with `curl` needs `--http1.1`. Over HTTP/2 there is
no `Upgrade` header to forward, so the request `curl` sends is not the one a
browser sends: browsers open a WebSocket on an HTTP/1.1 connection of its own,
whatever the page was loaded over.
