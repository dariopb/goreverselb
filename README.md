# Reverse tunnel Load Balancer
```
                                  _     ____  
 _ __ _____   _____ _ __ ___  ___| |   | __ ) 
| '__/ _ \ \ / / _ \ '__/ __|/ _ \ |   |  _ \ 
| | |  __/\ V /  __/ |  \__ \  __/ |___| |_) )
|_|  \___| \_/ \___|_|  |___/\___|_____|____/ 
```


reverselb is a L4 reverse tunnel and load balancer: it creates an encrypted TLS tunnel to an external ingress that in turns receives requests on the specified port and forwards the traffic to the client via encrypted, multiplexed sessions. Since it operates at L4 layer (tcp only for now), it allows to tunnel almost every protocol that runs on top of TCP (plain TCP, HTTP, HTTPS, WS, MQTT, SSH etc). It was inspired on services such as [ngrok](http://ngrok.com) and [inlets](https://github.com/inlets/inlets): those are great services but they either have limits or don't support TCP tunnels (and k8s LoadBalance for them) in their free tiers.

There is a client and a server components. The server is intended to be run on a machine/container that has a publicly accessible endpoint (there is an Azure ACI sample template below for quick deployment) while the client runs on the private network and configures the service that needs to be accesible externally.

The client (either via the cmd line application, library or container orchestrator extension) makes an TLS protected outbound connection to the reverselb server and configures the tunnel endpoints properties. The server now starts listening externally on the port the client instructed it to and when connections are received on this port, it relays the data back and forth between it and the backend connection. As many connections as desired can be established.


## Caddy application and modules

The repository also includes an explicitly assembled Caddy application in
`cmd/caddy-reverselb`. It imports standard Caddy, caddy-l4, and the goreverselb
modules; building it does **not** use `xcaddy`. The standalone
`go build ./cmd/goreverselb` command is unchanged.

```sh
go build -v ./cmd/caddy-reverselb
./caddy-reverselb list-modules
./caddy-reverselb adapt --config caddy/examples/dynamic.Caddyfile --adapter caddyfile --pretty
REVLB_TOKEN='replace-with-a-secret' ./caddy-reverselb run \
  --config caddy/examples/dynamic.Caddyfile --adapter caddyfile
```

All three modules now require Go 1.26 after the dependency refresh.
The Caddy application and plugin remain separate Go modules.
The checked-in `go.work` makes all three modules buildable from the repository
root. The workspace uses Go 1.26 and a combined dependency selection, without
adding Caddy requirements to the standalone `go.mod`. To build the standalone
module with only its own dependencies/toolchain settings, use
`GOWORK=off go build ./cmd/goreverselb`. Module-local builds also remain supported:
`cd cmd/caddy-reverselb && GOWORK=off go build .`.
The latest published Caddy and caddy-l4 releases are currently pinned at
`v2.11.4` and `v0.1.2`. Other dependencies are updated to compatible releases;
`automemlimit` stays at `v0.7.5` and `cel-go` at `v0.28.1` because newer versions
remove APIs used by Caddy. No Caddy fork is required.
The plugin is importable
as `github.com/dariopb/goreverselb/caddy`; local third-party builds can also use:

```sh
xcaddy build \
  --with github.com/dariopb/goreverselb/caddy=./caddy \
  --with github.com/dariopb/goreverselb=.
```

The default examples publish **HTTP-only frontends**, with automatic HTTPS
disabled and no public certificate requests. They run locally without DNS or
ACME setup. The tunnel control connection still uses TLS, as required by
existing goreverselb clients; its certificate is issued locally by Caddy's
internal CA. The sample disables installation of that CA into system/browser
trust stores. Use the local CA explicitly with `--tlscafile` when enabling
client certificate verification.

For a remote deployment, change `advertise_host` and the control TLS identity
and configure the appropriate certificate/trust policy. Supplied certificates
can use the Caddyfile `certificate` directive below or
`apps.tls.certificates.load_files` in native JSON. Public certificate
automation is opt-in and requires working ACME challenge routing or an
appropriate issuer.

**Dynamic publication:** only policy, templates, and a port pool are configured.
Clients register new services normally:

```sh
# With REVLB_TOKEN set, allocate an HTTP frontend for a local web server:
./goreverselb tunnel -e localhost:9999 -s web -b 127.0.0.1:8080

# Or publish HTTP on exactly port 8005:
./goreverselb tunnel -e localhost:9999 -s web-demo -p 8005 -b 127.0.0.1:8080
```

Open `http://localhost:8005/` for the explicit-port example. Raw TCP remains
supported by selecting an operator-configured `kind: tcp` template; the default
sample intentionally contains only the HTTP template.

The controller writes real HTTP/L4 servers, routes, and logical bindings to
Caddy's active JSON using conditional, atomic admin transactions. An endpoint
is acknowledged only after publication. Additional sessions reuse its port;
the last session leaving removes its generated configuration. Existing tunnel
sessions survive compatible publication reloads. Ports outside the pool,
conflicting requests, unsupported wrapping, and unauthorized services fail
registration instead of silently changing behavior.

Inspect the ordinary Caddy control plane:

```sh
curl http://127.0.0.1:2020/config/apps/goreverselb/generated
curl http://127.0.0.1:2020/config/apps/http/servers
curl http://127.0.0.1:2020/config/apps/layer4/servers
curl http://127.0.0.1:2020/goreverselb/status
```

The samples bind the admin listener to `0.0.0.0:2020`, so another machine can use
`http://<server-ip>:2020/config/`. The admin API has no homepage or web UI;
requesting `/` returns 404. **This API has full configuration access and
no built-in password authentication. Restrict port 2020 to trusted machines with
a firewall; do not expose it to the public Internet.**

The internal controller connects to `http://127.0.0.1:2020`; do not change
`publication.admin_endpoint` to `0.0.0.0`. That setting is a destination URL, not
the listener bind address. The controller supports a loopback HTTP endpoint or
`unix:///absolute/path/to/admin.sock` and never gives tunnel clients admin
credentials. Port 2020 avoids conflicting with another Caddy instance on the
standard admin port 2019. A loopback-specific listener can take precedence over
a wildcard listener on the same port, sending controller requests to the wrong
instance. If choosing another port, change both `admin` and
`publication.admin_endpoint` together.

Generated objects have stable `@id` values and must not be edited
independently of their ownership manifest. Change policy/templates instead.
The original Caddyfile is not rewritten. Active JSON can be autosaved/resumed
by Caddy, and publication intents are journaled in configured Caddy storage.
An unresolved publication reserves its port conservatively; only the same
authenticated service can recover it.

TCP templates support direct, SNI, and opt-in legacy instance dispatch. HTTP
templates use Caddy's regular `reverse_proxy` and support direct or Host
dispatch. A direct template allows one distinct instance per service, with
multiple sessions for that instance. Native JSON additionally supports
`allowed_sources`, operator-supplied `middleware`, and HTTP `upstream` settings.
Static `bindings_only` mode remains available for operator-defined shared
listeners. Consumer-facing `SSHWrap` is available through opt-in TCP templates
as described below. SSH remote-forward registration (`ssh -R`) in Caddy is
still unsupported; use the standalone server for that separate feature.

The full design and release acceptance criteria remain in
[spec-caddy-l4integration.md](spec-caddy-l4integration.md). This implementation
does not yet implement every operational requirement in that design: live
template changes for existing endpoints require disconnecting those endpoints
first; runtime-wide configurable drain deadlines, the full configurable limit
surface, and publication-specific Prometheus metrics remain outstanding.
Configuration transactions reload Caddy. Unknown publication outcomes retain
reservations conservatively rather than releasing potentially occupied ports.
Exhaustive version-matrix, capacity, crash-recovery, and shared-port validation
remain required before a production release. The current plugin's local-module
replacements are for checkout builds; published plugin distribution also
requires releasing the updated root module and pinning that release.

### Certificate files in one Caddyfile

Inside your existing `goreverselb` global block, add a certificate-chain file
and its matching private key:

```caddyfile
certificate /etc/ssl/certs/apps-fullchain.pem /etc/ssl/private/apps-key.pem
```

Use the **`goreverselb-caddyfile` adapter** with this directive. It first runs
Caddy's standard Caddyfile adapter, then merges the file pair into
`apps.tls.certificates.load_files`. There is still only one source Caddyfile;
no manual JSON editing, dummy site, static frontend listener, or Caddy fork is
needed. Existing regular sites, their certificate-selection tags, other
certificate loaders, and TLS/PKI policies are retained.

The directive is repeatable for multiple certificates. Quote paths containing
spaces. Absolute paths are recommended; relative paths are resolved from the
Caddy process's working directory. Standard Caddyfile environment substitutions
and imports remain supported. Only file paths appear in active configuration,
not the PEM contents. Caddy must have permission to read both files; keep the
private key restricted.

[certificates.Caddyfile](caddy/examples/certificates.Caddyfile) is a complete
example for `multi-1.apps.cloudexmaquina.com`, control port `9000`, and a pool
including frontend port `7445`. Set `REVLB_TOKEN` to your registration token,
then run:

```sh
export REVLB_CERT_FILE=/etc/ssl/certs/apps-fullchain.pem
export REVLB_KEY_FILE=/etc/ssl/private/apps-key.pem
./caddy-reverselb run \
  --config caddy/examples/certificates.Caddyfile \
  --adapter goreverselb-caddyfile
```

Register the service from the backend machine:

```sh
./goreverselb -t "$REVLB_TOKEN" tunnel \
  -e multi-1.apps.cloudexmaquina.com:9000 \
  -s multi-1 --frontendport 7445 --wraptls --insecuretls=false \
  -b 127.0.0.1:8080
```

Resolve the hostname to Caddy, then visit
`https://multi-1.apps.cloudexmaquina.com:7445`. A valid
`*.apps.cloudexmaquina.com` certificate covers both the control TLS identity and
the frontend. The example does not request new certificates; supplied files
are renewed externally. For a private CA, add `--tlscafile /path/to/ca.pem`
to the tunnel command and trust that CA on consumer machines.

After replacing certificate files, force a reload so unchanged file paths are
read again:

```sh
./caddy-reverselb reload \
  --config caddy/examples/certificates.Caddyfile \
  --adapter goreverselb-caddyfile \
  --address 127.0.0.1:2020 --force
```

Missing, malformed, or mismatched files fail Caddy provisioning instead of
silently falling back to certificate issuance. A rejected reload leaves the
existing configuration active. Reloading source policy may briefly interrupt
new frontend connections while reconciliation restores the recorded ports;
live tunnel sessions are retained. Using `--adapter caddyfile` with `certificate`
directives fails with guidance to select the extended adapter. Caddyfiles
without this directive still work with the standard adapter; native JSON still
uses Caddy's standard certificate loader directly.

### SSH-wrapped Caddy frontends

Add a rule and template inside the existing `publication dynamic` block:

```caddyfile
rule default@none ssh-* wrapped-ssh

template wrapped-ssh {
	kind tcp
	instance_dispatch direct
	frontend_ssh on
}
```

Keep the other templates and default unchanged. Reload Caddy, then register:

```sh
./goreverselb -l debug -t "$REVLB_TOKEN" tunnel \
  -e localhost:9999 -s ssh-web -b 127.0.0.1:8080 --wrapSSH
```

From the consumer machine, substitute the allocated frontend port for `8000`:

```sh
ssh -N -p 8000 -L 127.0.0.1:3000:localhost:8080 anyuser@192.168.1.166
```

Connecting to `http://127.0.0.1:3000` then reaches the registered backend.
Caddy/caddy-l4 owns the dynamic listener; the `goreverselb_ssh` handler terminates
SSH and sends each `direct-tcpip` channel through the ordinary L4 routing and
goreverselb session-selection path. The requested SSH destination is metadata,
not permission to dial arbitrary hosts: only the registered tunnel backend is
reachable. Multiple channels independently select tunnel sessions and preserve
half-closes. A connection permits up to 32 concurrent forwarding/session channels.

**As with standalone `--wrapSSH`, any username/password is accepted. This is
encryption, not consumer authentication or access control.** The registration
token still protects tunnel registration, not SSH consumer access. Restrict
exposure with a firewall or the template's `allowed_sources`. Passwords and
payloads are never logged. Passkey/token-based consumer authentication is not
implemented yet.

The Ed25519 host key persists in Caddy storage at
`goreverselb/<runtime_id>/ssh_host_key`, separately from the standalone
`revlb_ssh_host_key`. Verify the logged SHA256 host-key fingerprint before
accepting a new host identity. Do not expose or commit either private key.

`frontend_ssh` (also the native JSON field name) requires `kind tcp`; it cannot
be combined with `frontend_tls`. HTTP applications work through an SSH-wrapped
TCP template, rather than Caddy's HTTP reverse proxy. Both enabled and disabled
wrapping requests must match the selected template. `instance_dispatch sni`
matches TLS inside each decrypted SSH forwarding channel, while `legacy`
consumes its `PROXY->` or HTTP CONNECT preamble. Neither mode routes using the
SSH username or the `ssh -L` destination. A channel routing deadline closes
only that channel, not sibling channels. Existing service sessions must be
disconnected before changing their template.

Existing forwarding logs remain: source/frontend addresses, tunnel endpoints
and stream ID, backend addresses, directional bytes, duration and errors.
SSH adds username, channel ID and client-reported `ssh_origin` /
`ssh_destination`. The trusted `source_address` remains the actual SSH peer;
client-reported origin metadata is not used for source restrictions. Enable
Caddy debug logging for per-channel diagnostics as described below.

## Connection diagnostics

Use `-l debug` for the standalone server, tunnel client, or tunnel group:

```sh
./goreverselb -l debug -t "$REVLB_TOKEN" tunnel \
  -e localhost:9999 -s ddd -b localhost:9998
```

Debug logging includes connection setup, TLS verification mode, registration,
and retries. Once traffic reaches the assigned frontend, each stream includes
the original `source_address` (IP/port), frontend address/port, tunnel
`tunnel_local`/`tunnel_remote` addresses and `stream_id`, and the selected
backend's configured and actual local/remote addresses. Completion logs report
each copy direction, byte count, duration, and errors. Match the tunnel address
pair and stream ID to follow the connection across server and client logs.
Tokens and payloads are not logged.

The client flag does not change Caddy's logging level. For Caddy-side HTTP/L4
tunnel diagnostics, add `debug` to the Caddyfile's existing global options
block, or configure native JSON
`"logging": {"logs": {"default": {"level": "DEBUG"}}}`. L4 logs include the
frontend-to-tunnel hop and directional transfer counts; client logs show the
tunnel-to-backend hop. With HTTP keep-alive or HTTP/2, stream diagnostics describe
the pooled transport connection; use Caddy access logs for individual requests.
Until a consumer connects to the frontend, only setup/registration diagnostics
are expected.

## SNI/Host proxy loadbalancing

The reverselb server will try to do protocol identification in order to get a possible SNI/Hostname style redirection on the same tunnel port (to be able to share the same service port with multiple service instances). The load balancing is done on service instance names if multiple registrations for the same name/port are made.

The currently supported protos are: 
* HTTP **_connect_** protocol 
* Custom HA-PROXY like protocol (**_PROXY->_**[byte_len]**_instanceName_**[\n]) 
* HTTP **_Host_** header _[not yet added]_
* TLS ClientHello **_SNI_** extension

For example, to proxy multiple ssh servers on port 8001:

Using regular *connect* command:

```
ssh dario@instancename -o "ProxyCommand=connect -H localhost:8001 instancename 888"
```

Using embedded proxy support (executable option _stdinproxy_):

```
ssh dario@instancename -o "ProxyCommand=./goreverselb -l debug -t 0 stdinproxy -e localhost:8001"
# using TLS wrapping
ssh dario@instancename -o "ProxyCommand=./goreverselb -l debug -t 0 stdinproxy -e localhost:8000 --instancename %h -w -i"
```

# Console application

```
NAME:
   goreverselb - create tunnel proxies and load balance traffic between them

USAGE:
   goreverselb [global options] command [command options] [arguments...]

COMMANDS:
   server      runs as a server
   tunnel      creates an ingress tunnel
   stdinproxy  creates an stdin/stdout proxy to the endpoint
   help, h     Shows a list of commands or help for one command

GLOBAL OPTIONS:
   --loglevel value, -l value  debug level, one of: info, debug (default: "info") [$REVLB_LOGLEVEL]
   --token value, -t value     shared secret for authorization [$REVLB_TOKEN]
   --help, -h                  show help (default: false)
```

## Server

```
NAME:
   goreverselb server - runs as a server

USAGE:
   goreverselb server [command options] [arguments...]

OPTIONS:
   --port value, -p value                 port for the API endpoint (default: 0) [$REVLB_PORT]
   --autocertsubjectname value, -s value  subject name for the autogenerated certificate [$REVLB_AUTO_CERT_SUBJECT_NAME]
   --httpport value                       port for the HTTP rest endpoint (server will be disabled if not provided) (default: 0) [$REVLB_HTTP_PORT]
   --natsport value                       port for the secure NATS endpoint (server will be disabled if not provided) (default: 0) [$REVLB_NATS_PORT]
   --dynport value                        dynamic frontend port base (default: 8000) [$REVLB_DYN_FRONTEND_PORT]
   --dynportcount value                   number of dynamic frontend ports (default: 100) [$REVLB_DYN_FRONTEND_PORT_COUNT]
   --sshbackendport value                 port for SSH reverse backend connections (disabled if not provided) (default: 0) [$REVLB_SSH_BACKEND_PORT]
   --sshbackenduser value                 required SSH username for reverse backend connections (default: "reverseuser") [$REVLB_SSH_BACKEND_USER]
   --help, -h                             show help (default: false)
```

## SSH reverse backend

The server can accept standard SSH remote forwarding as an alternative to running the goreverselb client binary. Enable the dedicated SSH backend listener:

```bash
./goreverselb -t "0000" server -p 9999 -s localhost \
    --sshbackendport 2222
```

From the backend machine, publish its local SSH service on frontend port 8005:

```bash
ssh -NT \
    -o ExitOnForwardFailure=yes \
    -o ServerAliveInterval=30 \
    -o ServerAliveCountMax=3 \
    -R 8005:127.0.0.1:22 \
    reverseuser@SERVER -p 2222
```

Enter the server token (`0000` above) as the SSH password. The username defaults to `reverseuser` and can be changed with `--sshbackenduser` or `REVLB_SSH_BACKEND_USER`. Any other username is disconnected immediately.

Remote port `0` requests a dynamic frontend port from the configured pool:

```bash
ssh -NT -v -o ExitOnForwardFailure=yes \
    -R 0:127.0.0.1:22 reverseuser@SERVER -p 2222
```

The listener is public on `0.0.0.0` even if the `-R` bind address says `localhost`. Explicit and dynamic ports must belong to the configured dynamic frontend range. One SSH connection owns one frontend port; run another SSH connection for another port. Multiple SSH clients may register the same frontend port, and incoming connections are randomly balanced between them.

This backend listener supports password authentication and remote TCP forwarding only. It rejects shells, commands, local forwarding, public-key authentication, and instance routing. It is separate from the consumer-facing `--wrapSSH` option.

## Client

```
NAME:
   goreverselb tunnel - creates an ingress tunnel

USAGE:
   goreverselb tunnel [command options] [arguments...]

OPTIONS:
   --apiendpoint value, -e value      API endpoint in the form: hostname:port [$REVLB_API_ENDPOINT]
   --frontendport value, -p value     frontend port where the service is going to be exposed (endpoint will be apiendpoint:serviceport) (default: auto) [$REVLB_FRONTEND_PORT]
   --wraptls, -w                      always wrap the frontend connection as TLS (exposes HTTP backend as HTTPS endpoint) (default: false) [$REVLB_WRAP_TLS]
   --wrapSSH                          always wrap the frontend connection as SSH tunnel (default: false) [$REVLB_WRAP_SSH]
   --serviceendpoint value, -b value  backend service address (the local target for the lb: hostname:port) [$REVLB_SERVICE_ENDPOINT]
   --servicename value, -s value      service name string [$REVLB_SERVICE_NAME]
   --instancename value               instance name string (for SNI/Host functionality) (default: empty) [$REVLB_INSTANCE_NAME]
   --insecuretls, -i                  skip control server certificate verification (legacy default: true) [$REVLB_INSECURE_TLS]
   --tlscafile value                 PEM CA bundle for control server verification [$REVLB_TLS_CA_FILE]
   --tlsservername value             expected control server certificate name [$REVLB_TLS_SERVER_NAME]
   --help, -h                         show help (default: false)
```

For Caddy deployments, enable verified control TLS explicitly:

```sh
./goreverselb tunnel -e tunnels.example.com:9999 \
  --insecuretls=false -s web-demo -b 127.0.0.1:8080

# Private CA, or an IP endpoint whose certificate has a DNS name:
./goreverselb tunnel -e 192.0.2.10:9999 \
  --tlscafile /path/to/ca.pem --tlsservername tunnels.example.com \
  -s web-demo -b 127.0.0.1:8080
```

Both `tunnel` and `tunnelgroup` retain the historical insecure default for
compatibility with standalone self-signed servers. `--insecuretls=false`
verifies using system trust; specifying a CA file or server name enables
verification and rejects an explicitly conflicting `--insecuretls=true`.
The new library `NewMuxTunnelClientWithOptions` and
`NewMuxTunnelClientServiceGroupWithOptions` constructors verify by default.
Existing constructors retain legacy trust behavior.

## Tunnel Group
```
NAME:
   goreverselb tunnelgroup - creates multiple ingress tunnels

USAGE:
   goreverselb tunnelgroup [command options] [arguments...]

OPTIONS:
   --apiendpoint value, -e value   API endpoint in the form: hostname:port [$REVLB_API_ENDPOINT]
   --servicegroup value, -g value  service group json: like: '{"ssh1":{"name":"ssh1","ports":[{"port":8000,"protocol":"tcp","targetPort":22}],"backendIPs":["127.0.0.1"],"deleted":false}}' [$REVLB_SERVICE_GROUP_JSON]
   --insecuretls, -i                skip control server certificate verification (legacy default: true) [$REVLB_INSECURE_TLS]
   --tlscafile value                PEM CA bundle for control server verification [$REVLB_TLS_CA_FILE]
   --tlsservername value            expected control server certificate name [$REVLB_TLS_SERVER_NAME]
   --help, -h                      show help (default: false)
```

### Service Group JSON examples

Basic TCP tunnel:
```json
{
  "webserver": {
    "name": "webserver",
    "ports": [{"port": 8080, "protocol": "tcp", "targetPort": 80}],
    "backendIPs": ["127.0.0.1"],
    "deleted": false
  }
}
```

SSH-wrapped tunnel (use with `ssh -L` for local port forwarding):
```json
{
  "ssh-tunnel": {
    "name": "ssh-tunnel",
    "ports": [{"port": 8022, "protocol": "tcp", "targetPort": 22, "sshWrap": true}],
    "backendIPs": ["127.0.0.1"],
    "deleted": false
  }
}
```

TLS-wrapped tunnel (exposes HTTP backend as HTTPS):
```json
{
  "https-service": {
    "name": "https-service",
    "ports": [{"port": 8443, "protocol": "tcp", "targetPort": 80, "tlsWrap": true}],
    "backendIPs": ["192.168.1.100"],
    "deleted": false
  }
}
```

Multiple services:
```json
{
  "ssh1": {
    "name": "ssh1",
    "ports": [{"port": 8001, "protocol": "tcp", "targetPort": 22, "sshWrap": true}],
    "backendIPs": ["10.0.0.1"],
    "deleted": false
  },
  "ssh2": {
    "name": "ssh2",
    "ports": [{"port": 8002, "protocol": "tcp", "targetPort": 22, "sshWrap": true}],
    "backendIPs": ["10.0.0.2"],
    "deleted": false
  }
}
```

## Tunnel via SNI/Host
```
NAME:
   goreverselb stdinproxy - creates an stdin/stdout proxy to the endpoint

USAGE:
   goreverselb stdinproxy [command options] [arguments...]

OPTIONS:
   --serviceendpoint value, -e value  backend service address (hostname:port) [$REVLB_SERVICE_ENDPOINT]
   --instancename value               instance name string (for SNI/Host functionality) (default: empty) [$REVLB_INSTANCE_NAME]
   --wraptls, -w                      wrap the client connection on TLS (most likely the backend should be wrapped as well) (default: false) [$REVLB_WRAP_TLS]
   --insecuretls, -i                  allow skip checking server CA/hostname (default: false) [$REVLB_INSECURE_TLS]
   --help, -h                         show help (default: false)
```

## SSH Wrapped Tunnel (using wrapSSH)

When creating a tunnel with the `--wrapSSH` flag, the frontend connection is wrapped as an SSH tunnel. This allows users to connect to the exposed port using standard SSH local port forwarding (`ssh -L`), providing encryption for the consumer connection. The wrapper accepts any username/password and does not provide consumer access control.

### Setup

1. **Start the server** with an available frontend port:
```bash
./goreverselb -t "0000" server -p 9999 -s "localhost"
```

2. **Create a tunnel with SSH wrapping** on the client side:
```bash
./goreverselb -t "0000" tunnel --apiendpoint localhost:9999 \
    --servicename "myservice" \
    --serviceendpoint 127.0.0.1:8080 \
    --frontendport 8001 \
    --wrapSSH \
    --insecuretls=true
```

This exposes port 8001 on the server as an SSH endpoint that forwards traffic to the backend service at 127.0.0.1:8080.

3. **Connect using SSH local port forwarding**:
```bash
# Forward local port 3000 to the backend service through the SSH tunnel
ssh -N -L 3000:127.0.0.1:8080 anyuser@<server-address> -p 8001
```

- `-N`: Do not execute a remote command (only port forwarding)
- `-L 3000:127.0.0.1:8080`: Forward local port 3000 to the destination 127.0.0.1:8080 through the tunnel
- `anyuser`: Any username (password authentication accepts any password for the tunnel)
- `-p 8001`: The frontend port where the SSH tunnel is exposed

**Auto-reconnect**: If the server isn't ready yet or the connection drops, use a loop to keep retrying:
```bash
# Retry connection every 5 seconds until successful (Ctrl+C to stop)
while true; do ssh -N -L 3000:127.0.0.1:8080 anyuser@<server-address> -p 8001 -o ConnectTimeout=5 -o ServerAliveInterval=30; sleep 5; done
```

Or use `autossh` for a more robust solution (install via your package manager):
```bash
autossh -M 0 -N -L 3000:127.0.0.1:8080 anyuser@<server-address> -p 8001 -o ServerAliveInterval=30 -o ServerAliveCountMax=3
```

4. **Access the backend service** via your local port:
```bash
curl http://localhost:3000
```

### Environment Variables

When using environment variables:
```bash
export REVLB_WRAP_SSH=true
./goreverselb -t "0000" tunnel -e localhost:9999 -s "myservice" -b 127.0.0.1:8080 -p 8001
```

### Notes

- The SSH wrapper accepts any password for authentication (it's primarily for wrapping/encryption, not access control)
- Use the `-N` flag with ssh to avoid starting a terminal session (only port forwarding is supported)
- The destination address in `ssh -L` should match what the backend service expects

# Run it!

### Linux 

````
cd cmd/goreverselb
export GOPATH=/home/dario/go
go get github.com/rakyll/statik

go generate ./pkg/restapi
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -v ./cmd/goreverselb


# create a tunnel server on the local machine
./goreverselb -t "0000" server -p 9999 -s "localhost"

# register a forwarder to some random endpoint
./goreverselb -t "0000" tunnel --apiendpoint localhost:9999 --servicename "myendpoint-8888" --serviceendpoint ip.jsontest.com:80 --frontendport 8888 --insecuretls=true

# now hit the frontend on the port defined

curl --header "Host: ip.jsontest.com" http://localhost:8888
{"ip": "173.69.143.190"}

[notice that we need to override the host header since the tunnel is a plain TCP one so hitting servers that rely on the host header to find the target won't work without doing so]
````

### Raspberry PI

```
cd cmd/goreverselb
export GOPATH=/home/dario/go
go get github.com/rakyll/statik

go generate ./pkg/restapi
GOOS=linux GOARCH=arm GOARM=5 CGO_ENABLED=0 go build -v ./cmd/goreverselb

(running same a Linux)
```

## Docker (either linux, mac if you really want to try this way)

````
# server
docker run -it --rm --net host -e "REVLB_PORT=9999" -e "REVLB_TOKEN=0000" -e "REVLB_AUTO_CERT_SUBJECT_NAME=localhost" dariob/reverselb-alpine:latest ./goreverselb server

INFO[0000] goreverseLB version: latest
INFO[0000] Go Version: go1.13.5
INFO[0000] Go OS/Arch: linux/amd64
INFO[0000] CreateDynamicTlsCertWithKey: creating new tls cert for SN: [localhost]
INFO[0001] tunnel service listening on: tcp => [::]:9999


# client
docker run -it --rm --net host -e "REVLB_API_ENDPOINT=localhost:9999" -e "REVLB_TOKEN=0000" -e "REVLB_SERVICE_NAME=my service" -e "REVLB_FRONTEND_PORT=8888" -e "REVLB_SERVICE_ENDPOINT=ip.jsontest.com:80" -e "REVLB_INSECURE_TLS=true" dariob/reverselb-alpine:latest ./goreverselb tunnel

INFO[0000] goreverseLB version: latest
INFO[0000] Go Version: go1.13.5
INFO[0000] Go OS/Arch: linux/amd64
INFO[2020-01-12T18:21:40Z] NewTunnelClient to apiEndpoint [localhost:9999] with tunnel info: [{ my service 0000 {8888} 1 80 [ip.jsontest.com]}]


````

# Kubernetes LoadBalancer Operator

You can expose you kubernetes pods externally via a LoadBalancer operator available here: 


# Docker

### Create docker image Linux:

````
export GOPATH=/home/dario/go
go get github.com/rakyll/statik

go generate ./pkg/restapi
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -v ./cmd/goreverselb

sudo docker build -f docker/Dockerfile-alpine.txt -t dariob/reverselb-alpine .
sudo docker tag dariob/reverselb-alpine dariob/reverselb-alpine:0.1
sudo docker tag dariob/reverselb-alpine dariob/reverselb-alpine:latest
sudo docker push dariob/reverselb-alpine:latest
sudo docker push dariob/reverselb-alpine:0.1
````


### Raspberry PI 

````
export GOPATH=/home/dario/go
go get github.com/rakyll/statik

go generate ./pkg/restapi
GOOS=linux GOARCH=arm GOARM=5 CGO_ENABLED=0 go build -v ./cmd/goreverselb

sudo docker build -f docker/Dockerfile-pi.txt -t dariob/reverselb-pi .
sudo docker tag dariob/reverselb-pi dariob/reverselb-pi:0.1
sudo docker tag dariob/reverselb-pi dariob/reverselb-pi:latest
sudo docker push dariob/reverselb-pi:latest
sudo docker push dariob/reverselb-pi:0.1

````

# Azure ACI ingress sample

You can deploy a cheap entry point using Azure ACI container services (create a [free account](https://azure.microsoft.com/en-us/free/) if you don't have one to evaluate).
Create a new template deployment using the template below (make sure that dnsNameLabel and the autocertsubjectname strings match the name and region where you are deploying).

```json
{
    "$schema": "https://schema.management.azure.com/schemas/2015-01-01/deploymentTemplate.json#",
    "contentVersion": "1.0.0.0",
    "parameters": {
        "containerGroupName": {
          "type": "string",
          "defaultValue": "reverselbdefaultname",
          "metadata": {
            "description": "reverseLB server"
          }
        },
        "containerImageName": {
            "type": "string",
            "defaultValue": "dariob/reverselb-alpine:latest",
            "metadata": {
              "description": "reverseLB image"
            }
          }
  
          ,"port": {
            "type": "string",
            "defaultValue": "9000",
            "metadata": {
              "description": "API endpoint port"
            }
          }
          ,"httpport": {
            "type": "string",
            "defaultValue": "9001",
            "metadata": {
              "description": "HTTP endpoint port"
            }
          }
          ,"natsport": {
            "type": "string",
            "defaultValue": "9002",
            "metadata": {
              "description": "NATS endpoint port"
            }
          }
          ,"token": {
            "type": "string",
            "defaultValue": "",
            "metadata": {
                "description": "shared secret for authorization"
            }
        }
        ,"autocertsubjectname": {
            "type": "string",
            "defaultValue": "reverselb-123.westus2.azurecontainer.io",
            "metadata": {
                "description": "subject name for the autogenerated certificate"
            }
        }
        ,"dnsNameLabel": {
            "type": "string",
            "defaultValue": "reverselb-123",
            "metadata": {
                "description": "Dns name prefix for pod"
            }
        }
        ,"loglevel": {
            "type": "string",
            "defaultValue": "debug",
            "metadata": {
                "description": "loglevel"
            }
        }
        ,"portserv1": {
            "type": "string",
            "defaultValue": "8888",
            "metadata": {
              "description": "service port 1"
            }
          }
          ,"portserv2": {
            "type": "string",
            "defaultValue": "8889",
            "metadata": {
              "description": "service port 1"
            }
          }
    },
    "variables": {
        "reverselbimage": "dariob/reverselb-alpine:latest"
    },
    "resources": [
        {
            "name": "[parameters('containerGroupName')]",
            "type": "Microsoft.ContainerInstance/containerGroups",
            "apiVersion": "2018-10-01",
            "location": "[resourceGroup().location]",
            "properties": {
                "containers": [
                    {
                        "name": "reverselbdefaultname",
                        "properties": {
                            "image": "[parameters('containerImageName')]",
                            "environmentVariables": [
                                {
                                    "name": "PORT",
                                    "value": "[parameters('port')]"
                                },
                                {
                                    "name": "LOGLEVEL",
                                    "value": "[parameters('loglevel')]"
                                },
                                {
                                    "name": "TOKEN",
                                    "value": "[parameters('token')]"
                                },
                                {
                                    "name": "AUTO_CERT_SUBJECT_NAME",
                                    "value": "[parameters('autocertsubjectname')]"
                                },
                                {
                                    "name": "OWN_CONTAINER_ID",
                                    "value": "[resourceId('Microsoft.ContainerInstance/containerGroups', parameters('containerGroupName'))]"
                                }
                            ],
                            "resources": {
                                "requests": {
                                    "cpu": 1,
                                    "memoryInGb": 1
                                }
                            },
                            "ports": [
                                {
                                    "port": "[parameters('port')]"
                                }
                                ,{
                                    "port": "[parameters('portserv1')]"
                                }
                                ,{
                                    "port": "[parameters('portserv2')]"
                                }
                            ]
                        }
                    }

                ],
                "osType": "Linux",
                "ipAddress": {
                    "type": "Public",
                    "ports": [
                      {
                        "protocol": "tcp",
                        "port": "[parameters('port')]"
                      }
                      ,{
                        "protocol": "tcp",
                        "port": "[parameters('portserv1')]"
                      }
                      ,{
                        "protocol": "tcp",
                        "port": "[parameters('portserv2')]"
                      }
                    ],
                    "dnsNameLabel": "[parameters('dnsNameLabel')]"
                }
            }
        }
    ]
}
```

# Embedding the client

Client construction starts background connection/retry workers; a successful
constructor return does **not** mean the server has accepted the tunnel.
Use `WaitReady(ctx)` to wait for registration and retrieve the allocated port,
or `Status()` to query a thread-safe snapshot at any time:

```go
import (
	"context"
	"fmt"
	"time"

	tunnel "github.com/dariopb/goreverselb/pkg"
)

func runTunnel(ctx context.Context, token string) error {
	td := tunnel.TunnelData{
		ServiceName:          "web",
		Token:                token,
		BackendAcceptBacklog: 1,
		FrontendData: tunnel.FrontendData{
			Port: 0, // ask the server to allocate a frontend port
		},
		TargetPort:      8080,
		TargetAddresses: []string{"127.0.0.1"},
	}
	tc, err := tunnel.NewMuxTunnelClient("localhost:9999", td)
	if err != nil {
		return err
	}
	defer tc.Close()

	readyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	status, err := tc.WaitReady(readyCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("waiting for tunnel (state=%s, last error=%q): %w",
			status.State, status.LastError, err)
	}
	if status.PublicationMode == "bindings_only" {
		fmt.Println("Tunnel binding ready; no dedicated frontend port")
	} else {
		fmt.Printf("Tunnel ready at %s (allocated port %d)\n",
			status.FrontendAddress, status.FrontendPort)
	}

	// Run your application until shutdown; the deferred Close stops the client.
	<-ctx.Done()
	return ctx.Err()
}
```

For monitoring while the application runs:

```go
status := tc.Status()
fmt.Printf("state=%s ready=%d/%d frontend=%q last_error=%q\n",
	status.State, status.ReadyConnections, status.DesiredConnections,
	status.FrontendAddress, status.LastError)
if status.State == tunnel.ClientStateReady {
	// At least one control connection has an accepted registration.
}
```

| State | Meaning |
|---|---|
| `connecting` | Connecting to the control endpoint, including its TLS handshake |
| `registering` | TLS connected; waiting for the server to accept registration |
| `ready` | At least one control connection has an accepted registration |
| `reconnecting` | No ready/connecting/registering workers; waiting to retry after failure |
| `closed` | Shutdown requested; `Close()` waits for all worker/stream cleanup |

`ConnectedConnections` counts completed TLS handshakes, including registrations
still pending. `ReadyConnections` counts accepted registrations. With a backlog
greater than one, a single failed connection does not mark the entire client
unready: the aggregate state prefers ready, then registering, then connecting,
then reconnecting. `Connections` contains an independent per-worker snapshot,
with stable zero-based IDs, state, frontend metadata, timestamps and errors.
`LastError` is the most recent outstanding connection-attempt error; successful
registration clears the corresponding worker's error. Tokens are not included
in status, and echoed tokens are redacted from connection errors.

`FrontendAddress` is an advertised `host:port`, falling back to the control
endpoint's host when a standalone server does not advertise one. `FrontendPort`
is the **server-confirmed** port, not merely the requested port. Aggregate
frontend fields come from the lowest-ID ready connection; they are cleared
when no connection is ready and refreshed after reconnection. Publication mode
is `"dynamic"` or `"bindings_only"` for Caddy, and may be empty for standalone
or older servers. A bindings-only registration is ready with port `0` and no
frontend address. The older `tc.FrontendPort()` accessor remains compatible:
it can contain the requested or last-reported port and is not a readiness check.

`WaitReady` supports simultaneous callers without polling or callbacks. It
continues waiting through retries; timeout/cancellation returns `ctx.Err()`,
and client closure returns `net.ErrClosed`, together with a current snapshot.
Cancelling a wait does not stop the client; call `Close()` for that. A ready
snapshot describes control-plane registration at that instant, not backend
health or a guarantee that a subsequent request cannot fail. Network loss is
reflected when detected by I/O or yamux keepalives, not by an independent
health probe.

`NewMuxTunnelClient` retains historical insecure control-TLS compatibility.
Use `NewMuxTunnelClientWithOptions` with verified TLS for production; both
constructors expose the same status API.
