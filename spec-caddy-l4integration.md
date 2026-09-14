# Caddy integration for goreverselb dynamic tunnels

**Status:** Design target with a working core implementation. The repository now
includes the shared tunnel core, dynamic Caddy publication, HTTP/L4 adapters, and
the explicitly assembled `cmd/caddy-reverselb` application. The complete release
criteria below are not all satisfied; see the implementation notes in
`README.md` and the executable examples in `caddy/examples/`. Proposed syntax in
this document is not, by itself, a promise that every option is implemented.

## 1. Objective and scope

Make goreverselb's dynamically registered reverse tunnels available as upstream
connections inside an ordinary, extensible Caddy binary. Support both:

- HTTP routing through Caddy's standard HTTP app and `reverse_proxy` handler,
  including automatic HTTPS, HTTP matchers, middleware, and streaming.
- TCP routing through caddy-l4's servers, matchers, handler chains, and listener
  wrappers, including TLS passthrough and optional TLS termination.

**Registration-driven endpoint publication is the primary mode.** An authorized
client registers a service and requests either port `0` (allocate a new port) or
a specific port. The control plane allocates/reserves the port, renders an
operator-approved template, and commits actual HTTP or L4 listener/route objects
to Caddy's active configuration. The client receives the published port only
after activation succeeds. Operators configure policy, port ranges, and
templates, not each service or endpoint in advance. Generated configuration
MUST be visible through Caddy's standard `GET /config/` API.

The integration must compile as an independently importable Caddy module usable
with `xcaddy`. Also provide a complete Caddy application in
`cmd/caddy-reverselb`, built with ordinary Go commands and explicit module
imports, without using `xcaddy`. Neither distribution requires a fork of Caddy
or caddy-l4. Preserve the existing
standalone `server`, `tunnel`, `tunnelgroup`, and `stdinproxy` commands, library
entry points, and existing wire protocol. Caddy is an additional server host,
not a replacement executable or a new requirement for tunnel clients.

Initial tunnel transport remains TCP + TLS + yamux. UDP tunneling, QUIC
passthrough, distributed session replication, automatic publication of arbitrary
client-requested hostnames, and an HTTP forward proxy are out of scope. Caddy may
still accept HTTP/3 on its normal HTTP listeners and proxy those HTTP requests
over a TCP tunnel; this is not UDP tunnel support.

Normative terms such as MUST and SHOULD describe implementation requirements.
All new module IDs, Go APIs, and configuration fields below are proposed.

## 2. Current implementation and required separation

The production CLI is `cmd/goreverselb`, not `cmd/main.go`. The current
implementation has useful functionality but cannot simply be instantiated from
a Caddy handler:

| Surface | Current behavior | Required integration boundary |
|---|---|---|
| `pkg/muxtunnel.go` | `NewMuxTunnelService` opens a TLS listener, loads/creates an SSH key, and starts goroutines | Explicit construction, start, stop, and injected host resources |
| Registration | The first yamux stream carries JSON `TunnelData`; authorization and frontend allocation precede session registration | Separate authentication, session registry, and publication policy |
| Service registry | User -> service -> instance -> yamux session maps live inside frontend runtime data | Registry independent of any frontend listener or port |
| Frontend proxy | `doProxy` selects a session, opens a stream, sends metadata, and copies bytes | A reusable stream dialer, distinct from sniffing and byte copying |
| Client | Receives framed `TunnelConnecData`, dials its local target, and proxies bytes | Preserve this data path and client-local backend selection |
| Standalone routing | SNI, custom `PROXY->`, and HTTP CONNECT sniffing | Keep as a standalone adapter; do not apply it implicitly to Caddy traffic |
| SSH reverse backend | `pkg/sshReverseBackend.go` supports standard SSH remote forwarding through separately tracked frontends | Preserve standalone support; Caddy exposure is a later, explicit adapter |

Today registration, port allocation, frontend wrapping, and backend lifetime are
coupled. Separate them so dynamic port allocation drives Caddy-owned listeners
instead of standalone listeners. Optional shared-port routing must also work
without a public port per service. Neither mode requires a loopback TCP hop.

The current constructor also contains process-level logging behavior, and the
standalone startup owns `user_store.yaml` and `revlb_ssh_host_key`. Embedded mode
must not call this startup path, write these working-directory files, invoke
`log.Fatal`, or start the optional REST/NATS services as a side effect.

## 3. Architecture

```text
Existing tunnel / tunnelgroup clients
        |
        | outbound TLS + yamux; first stream authenticates/registers
        v
Caddy app: goreverselb
        |
        +-- credential / registration policy
        +-- port allocator + template renderer + config reconciler
        |       |
        |       +-- Caddy admin config transaction -> HTTP / L4 listeners
        +-- live registry: user -> service -> instance -> sessions
        +-- stream dialer and session lifecycle
                   ^                           ^
                   |                           |
HTTP reverse_proxy transport           caddy-l4 tunnel handler
                   ^                           ^
                   |                           |
Caddy HTTP routes / HTTPS             L4 routes / TLS / raw TCP
                   ^                           ^
                   |                           |
             HTTP consumers              TCP consumers
```

There is one logical registry per configured runtime ID. HTTP and L4 modules
resolve that registry through the Caddy app. A client joining, reconnecting, or
leaving changes registry membership immediately. The first registration of an
endpoint creates its Caddy configuration; the last session leaving schedules
removal. Additional sessions for an already published endpoint need no config
change. Users never need to edit a Caddyfile or call the admin API themselves.

Clients initiate endpoint publication and own their private backend targets.
Operator configuration constrains allowed ports, templates, identities, and
TLS names. A controller inside the module translates authorized registrations
into native Caddy configuration using the supported admin API. Only Caddy's
HTTP/L4 apps bind the generated public listeners; a private listener map plus a
custom status endpoint is not an acceptable substitute.

### 3.1 Framework-independent core

Extract reusable code within the root Go module, keeping it free of Caddy and
caddy-l4 imports. Suggested boundaries:

```text
pkg/tunnelcore/             registry, authorization, lifecycle, stream dialing
pkg/tunnelcore/protocol/    existing control JSON and data framing
pkg/                       existing public API and standalone frontend adapters
caddy/                     separate Go module: app and registration bundle
caddy/controller/          publication leases, templates, config reconciliation
caddy/httptransport/       HTTP reverse_proxy transport
caddy/l4/                  L4 terminal handler and legacy routing adapter
cmd/caddy-reverselb/        separate Go module: explicitly assembled Caddy binary
```

Preserve exported types and constructors through aliases or compatibility
wrappers where appropriate. Internal package layout may change, but callers
must not need to rewrite existing client or standalone setup code.

An illustrative core contract is:

```go
type Selector struct {
    UserID   string
    Service  string
    Instance string
}

type ConnectionInfo struct {
    SourceAddress string
}

type Dialer interface {
    DialContext(context.Context, Selector, ConnectionInfo) (net.Conn, error)
}
```

The final API also needs explicit lifecycle methods, immutable registry
snapshots/events, and typed errors such as no available session, admission
denied, resource exhausted, and stream-open timeout.

`DialContext` MUST:

1. Authorize access to the selected binding and snapshot eligible sessions under
   short-lived locks; never hold registry locks during network I/O.
2. Select a live session, open a new stream, and send exactly one existing
   `TunnelConnecData` frame before exposing the connection to the caller.
3. Respect cancellation and bounded stream-open/metadata-write timeouts. If the
   yamux API cannot cancel a pending open directly, bound outstanding opens and
   close any stream that arrives after cancellation; do not leak goroutines or
   close an unrelated caller's entire session.
4. Close partially initialized streams on failure and return an explicit error.
5. Return a connection whose deadlines and close behavior are suitable for both
   HTTP transports and bidirectional TCP forwarding.

Opening a stream is not proof that the client successfully dialed its local
backend: the existing protocol has no backend-dial acknowledgment. Do not report
backend application health or transparently replay traffic based on that
assumption.

## 4. Caddy modules and distribution

| Module ID | Role | Host interface |
|---|---|---|
| `goreverselb` | Control listener, publication controller, policy, bindings, and registry lifecycle | `caddy.App` |
| `http.reverse_proxy.transport.goreverselb` | HTTP round trips over registered tunnel streams | `http.RoundTripper` |
| `layer4.handlers.goreverselb` | Terminal forwarding of an L4 connection into a tunnel | `layer4.NextHandler` |
| `layer4.handlers.goreverselb_route` | Opt-in legacy instance preamble/CONNECT dispatch | `layer4.NextHandler` |
| `layer4.handlers.goreverselb_ssh` | Opt-in consumer SSH local-forward wrapping ahead of L4 instance routing | `layer4.NextHandler` |
| `caddy.adapters.goreverselb-caddyfile` | Standard Caddyfile adaptation plus native TLS certificate-file merging | `caddyconfig.Adapter` |

Use `caddy.RegisterModule`, `CaddyModule`, provisioning, validation, cleanup, and
compile-time interface assertions. Obtain structured loggers from the Caddy
context. Register Caddyfile support without replacing built-in HTTP directives.

Use a nested Go module at `github.com/dariopb/goreverselb/caddy` so standalone
builds do not acquire Caddy's dependency graph or toolchain requirements. Its
root package is an import bundle for the app, HTTP transport, L4 integration,
and required caddy-l4 registrations. Do not register another `layer4` app or
duplicate upstream modules. Subpackages may be imported separately by custom
builds, but the bundle is the documented default.

### 4.1 Importable plugin

Third-party users may continue to assemble their own Caddy distribution.
Proposed released plugin build:

```sh
xcaddy build --with github.com/dariopb/goreverselb/caddy@<release>
./caddy list-modules
```

For development, from the repository root:

```sh
xcaddy build \
  --with github.com/dariopb/goreverselb/caddy=./caddy \
  --with github.com/dariopb/goreverselb=.
```

Publish nested-module tags as `caddy/vX.Y.Z`; consumers request the corresponding
`vX.Y.Z` module version. Commit a reproducible dependency set, pin the tested
Caddy release and caddy-l4 revision, and document the required Go version.
Caddy-l4 describes itself as experimental; compatibility with arbitrary future
revisions is not promised. CI must test the declared supported version matrix,
not only whatever `latest` resolves to.

### 4.2 Repository-provided Caddy application

Provide `cmd/caddy-reverselb/main.go` as a checked-in, explicit composition root
for the full Caddy application. It must bind standard Caddy modules, caddy-l4,
and the goreverselb integration through Go imports and invoke Caddy's ordinary
CLI entry point. It is not a wrapper around `xcaddy`, a generated temporary main,
or a reduced server with a separately implemented CLI.

Required entry-point shape:

```go
package main

import (
    _ "time/tzdata"

    caddycmd "github.com/caddyserver/caddy/v2/cmd"
    _ "github.com/caddyserver/caddy/v2/modules/standard"
    _ "github.com/dariopb/goreverselb/caddy"
    _ "github.com/mholt/caddy-l4"
)

func main() {
    caddycmd.Main()
}
```

The standard module import preserves normal HTTP/TLS functionality and
Caddyfile support; the other explicit imports register L4 and goreverselb
modules. Go initializes each package once even when the integration bundle also
imports caddy-l4. Do not duplicate module registration code in this executable.

Keep this directory in its own Go module,
`github.com/dariopb/goreverselb/cmd/caddy-reverselb`, with committed `go.mod` and
`go.sum`. This preserves the root standalone module's Caddy-independent
dependency graph and toolchain. For repository builds, declare explicit local
replacements for `github.com/dariopb/goreverselb => ../..` and
`github.com/dariopb/goreverselb/caddy => ../../caddy`. Dependencies' own replace
directives do not propagate, so both belong in the application module.
Pin Caddy/caddy-l4 versions consistently with the integration module.

Required build/run workflow from the repository root:

```sh
go build -v ./cmd/caddy-reverselb
./caddy-reverselb list-modules
./caddy-reverselb run --config /path/to/Caddyfile --adapter caddyfile
```

No `xcaddy` installation, code generation, or manual workspace setup is required.
Check in a root `go.work` that includes `.`, `./caddy`, and
`./cmd/caddy-reverselb` so this root-directory command works despite the nested
module boundaries. The workspace uses Go 1.26 and combined dependency
selection; it does not add Caddy requirements to the standalone `go.mod`.
`GOWORK=off go build ./cmd/goreverselb` preserves standalone-only dependency and
toolchain selection. Independent nested-module builds must also continue to
work with `GOWORK=off` from their own directories.

The resulting binary must expose normal Caddy commands, including `run`,
`start`, `stop`, `reload`, `adapt`, `validate`, and `list-modules`, and the normal
admin API. Its HTTP/L4/dynamic-publication behavior must be the same as a
third-party Caddy binary importing the integration bundle. Distribute it as
`caddy-reverselb`, separately from the standalone `goreverselb` executable.

## 5. Registration, bindings, and compatibility

### 5.1 Endpoints and logical bindings

An endpoint is a publication lease keyed by `(runtime_id, user_id, service)`,
consistent with the current service-level frontend port. It owns a port,
template selection/revision, generated configuration IDs, and one or more
instance bindings. A binding maps a generated, stable name to an exact
`(user_id, service, instance)` selector. An omitted instance means the empty
instance, not all instances. Preserve random selection among sessions under an
exact selector and existing client-local backend selection.

The controller creates endpoint leases and bindings from authorized
registrations; they do not need to be predeclared. Additional registrations for
the same endpoint reuse its port and routing policy. Port `0` reuses an existing
lease, and a nonzero request must match that lease or fail. A different user or
service cannot claim the same listening address. Instance registrations add
routes/bindings on that service's listener, not additional frontend ports.

Templates must define instance dispatch: an exact direct binding for a
single-instance endpoint, or permitted SNI/HTTP Host/legacy instance mappings
for multiple instances. A direct-only template rejects a second distinct
instance rather than changing the destination of existing traffic. Mapping
entries are generated from authenticated registrations and template policy.
They must never allow arbitrary request headers to select an unregistered
tenant or service. Unknown names fail closed.

Operator-defined bindings remain an optional mechanism for shared static
routes. Such bindings may exist with zero sessions and need not allocate ports.

### 5.2 Publication policies

Support two explicit server-side modes:

| Mode | Registration result |
|---|---|
| `dynamic` | Allocates/honors a requested port and publishes a template-derived native Caddy HTTP/L4 endpoint |
| `bindings_only` | Registers sessions for operator-defined bindings; no per-service listener or port allocation |

`dynamic` is the Caddy default and is required for the first usable release.
The standalone server retains its existing frontend behavior and CLI defaults.
Dynamic Caddy frontends are not a legacy fallback.

Operators configure a bind address and port pool
`[port_start, port_start + port_count)`. Both automatic and explicit ports must
belong to the permitted pool. Explicit requests either receive exactly that
port or an error; never substitute another port silently. Allocation checks
reserved leases and configured HTTP/L4/control/SSH listeners, including wildcard
and IPv4/IPv6 overlap; Caddy activation is the final authority for OS-level bind
conflicts. Reserve logically, then let Caddy bind; do not occupy a socket with a
temporary listener that prevents Caddy from opening it.

Templates decide HTTP versus raw TCP; existing `TunnelData` does not identify
the backend application protocol. Select templates using ordered
operator-defined rules over authenticated user/service/instance and supported
frontend flags, with an explicit default. Old clients need no new fields.
An optional additive template hint from newer clients is only a request and
must be authorized; clients cannot submit raw Caddy configuration.

The selected template must explicitly support requested `TLSWrap`/`SSHWrap`
semantics or registration fails. TLSWrap can map to a Caddy TLS-termination
template with an authorized certificate name. SSHWrap maps to an explicitly
enabled `frontend_ssh` TCP template and the `goreverselb_ssh` L4 handler.
Wrapping flags must match the template in both directions. Reject SSH wrapping
on HTTP templates and combined frontend TLS/SSH wrapping rather than silently
omitting a layer. Standalone SSH wrapping remains unchanged.

The SSH handler owns no listener: generated Caddy L4 routes place it before a
subroute containing the ordinary direct, SNI, or legacy instance routes.
Every accepted `direct-tcpip` channel gets independent matcher state and a
tunnel stream selected by the existing registry. SNI/legacy dispatch examines
the decrypted channel payload, not the SSH username or requested forwarding
destination. Destination metadata must never authorize arbitrary outbound
dials. Half-closes, cancellation, bounded channel concurrency, and per-channel
routing deadlines must be preserved; a channel deadline must not close sibling
channels. Compatible publication reloads preserve live SSH connections and
their existing forwarding streams.

Persist an Ed25519 host key per runtime in Caddy storage under
`goreverselb/<runtime_id>/ssh_host_key`, using storage locking for creation.
Never replace a corrupt stored key silently. Log its public fingerprint, not
key material. Preserve the standalone encryption-only behavior: any supplied
username/password is accepted, with no claim of consumer authentication.
Passkey/token-based authentication is future work. Registration authentication
is independent and still mandatory. Restrict sources before the SSH handshake
using the actual SSH peer, not client-reported origin metadata. Preserve all
existing connection/stream/copy diagnostics and add SSH peer, user, channel,
origin and destination context without logging passwords, tokens or payloads.

Send `TunnelDataResponse.FrontendPort` with the actual allocated/requested port
only after the generated Caddy configuration is committed and ready to accept
traffic. Populate `FrontendAddress` from an operator-configured advertised
address, not a wildcard bind address. Optional response fields can describe
publication mode and HTTP URLs without changing old clients.

In `bindings_only`, accept existing clients requesting frontend port `0` and
return `FrontendPort: 0`. Reject nonzero port requests and `TLSWrap`/`SSHWrap`
requests with a useful registration error rather than silently pretending those
features were applied. TLS termination belongs to the configured Caddy route.
Existing `tunnelgroup` configurations requesting explicit frontend ports use
`dynamic` mode without changing that request convention.

Add optional response fields describing publication mode and public endpoints
for upgraded clients. Old clients ignore those fields; their existing readiness
log may display `host:0` in bindings-only mode. Document this limitation, and make
updated clients display logical/public endpoints instead. Their reconnect logic
must retain port `0` for bindings-only registrations. Do not return a shared
HTTP listener's port as though the client had allocated a service frontend.

### 5.3 Templates and native configuration transactions

A template is an operator-authored, validated blueprint for a Caddy HTTP server
or L4 server, its routes/matchers/handlers, and any permitted TLS configuration.
Initial typed templates provide `tcp` and `http` kinds, a direct or mapped
instance-dispatch policy, timeouts, source restrictions, and HTTP middleware/
backend transport settings. Advanced templates may include module JSON, but
must retain normal Caddy validation and explicit ownership boundaries.

Substitution is typed JSON construction, not string substitution into arbitrary
JSON or Caddyfile text. Only validated fields such as allocated address/port,
binding ID, and permitted hostname may be expanded. User tokens, arbitrary
filesystem paths, admin settings, and unapproved certificate names are never
template inputs. The generated backend dial target always remains the tunnel.

For a raw TCP endpoint, generate a server in `apps.layer4.servers` with its
`listen` address and terminal goreverselb route(s). For HTTP, generate a server
in `apps.http.servers` with normal HTTP routes and `reverse_proxy` using the
tunnel transport. HTTP templates explicitly select plaintext or TLS; automatic
HTTPS must not accidentally publish additional unapproved listeners. Allocated
HTTP ports are real HTTP listeners, not opaque TCP proxies labeled HTTP.

Use a single serialized reconciler per runtime for publication changes. A
registration joining an unchanged active binding only validates the current
lease and admits the new session; it bypasses config mutation. When
materialization is required, implement:

1. Authenticate registration, apply policy, choose a template, and reserve or
   reuse a lease. Assign a stable endpoint ID and idempotent operation ID.
2. Record the session as pending; construct the generated bindings and
   HTTP/L4/TLS objects. Pending sessions cannot serve consumer traffic.
3. `GET /config/`, retaining its ETag. Merge only controller-owned objects into
   this current document, preserving unrelated operator/module configuration.
4. Commit the complete candidate with one `POST /config/` and `If-Match` using
   that ETag. This is one Caddy transaction across all affected apps; do not
   issue a sequence of partially visible per-route/per-binding updates.
5. On HTTP 412, reread and rebase with bounded retries. On validation/bind
   failure, retain Caddy's old config, roll back pending registration and lease,
   and return a control error. Do not overwrite concurrent operator edits.
6. On success, confirm the operation's generated objects are active, admit the
   pending session, and send the response. There may be a short fail-closed
   interval between listener activation and session admission, never premature
   success to the registering client.

For TLS templates, a successful config load alone does not prove that
asynchronous certificate issuance has completed. Require a usable certificate
under the configured policy before acknowledging a TLS endpoint, or fail the
publication within its deadline and reconcile its generated objects. Never
report a TLS-wrapped endpoint as ready while only its TCP socket is available.

The reconciler is internal to the module but uses the supported local admin
config API; clients do not need admin access. Require an operator-configured
loopback or permission-restricted Unix-socket admin endpoint in dynamic mode,
including any required admin trust settings. An admin-disabled deployment must
fail clearly rather than silently create invisible listeners. Never expose or
grant Caddy admin credentials to tunnel users.

Current public `caddy.Load` performs an unconditional replacement and does not
provide the ETag compare-and-swap contract above. Do not use it for a stale
read/modify/write, access private Caddy config globals, or mutate live app
structs and claim those mutations will appear in `GET /config/`.

Do not call the admin API synchronously from `Provision`, `Start`, `Stop`, or
`Cleanup`, or wait there for a reconciler that is waiting on config activation.
Caddy reload holds config locks while running lifecycle hooks. A long-lived
shared controller queues work outside those hooks, survives its own successful
reloads, and does not restart/requeue identical publications on every load.

A config request timeout or lost reply has an **unknown**, not failed, outcome.
Read back by stable IDs/operation revision before deciding whether to admit,
remove, or retry. Keep the port reservation until the result is resolved. If
the client disconnects or response delivery fails after commit, schedule
compensating cleanup only when no other session owns the endpoint.

### 5.4 Ownership and control-plane visibility

Generated servers/routes use a reserved name prefix and stable Caddy `@id`
values, for example `revlb-edge-e42-server`. The controller also writes an
ownership manifest under `apps.goreverselb.generated`, containing endpoint ID,
user/service, binding selectors, allocated port/address, template/revision,
operation revision, and generated object IDs. Bindings in that manifest are
resolvable by the HTTP/L4 modules during candidate provisioning without
depending on application start order.

The manifest describes publication, not live session objects. Do not serialize
tokens, sockets, session counts, or last-seen timestamps into Caddy config.
`GET /config/` and narrower queries MUST show the actual generated HTTP/L4
configuration as well as this manifest. `/id/<generated-id>` provides stable
lookup. An optional admin status extension may show pending operations, session
counts, and errors, but is supplementary; it cannot replace config visibility.

Only reconcile owned IDs and verify their recorded revision/content before
overwriting or deleting them. Templates and policy are operator-owned.
Operators change exposure through those settings; direct edits to generated
objects are reported as ownership conflicts and suspend automatic replacement
of the affected endpoint, not silently overwritten. Revoked endpoint admission
still fails closed while conflicting config is resolved.

Additional sessions and normal keepalives do not rewrite the manifest. Only
publication changes (including endpoint/instance routing and template/policy
updates) cause config transactions. Bound/coalesce
registration bursts; report pending publication rather than claiming that
dynamic endpoints have no reload cost.

### 5.5 Wire and standalone guarantees

Keep the TLS/yamux stream ordering, existing control JSON names, token/user
convention, service/instance convention, and two-byte little-endian data-frame
prefix. Additive response fields do not require old clients to change.

Fix framing in the shared core using full reads and complete writes, with a
1000-byte maximum outgoing metadata payload while old receivers are supported.
Reject oversized payloads before narrowing to `uint16`. Wire-compatible fixes
do not make old clients' fragmented-read handling reliable; upgraded clients are
the recommended deployment, and this limitation must be recorded in the
compatibility matrix.

Validate identifier lengths and ambiguous delimiters in new Caddy
configuration, preserving valid existing wire identities. Do not silently
reinterpret malformed identifiers into another user's service.

The standalone SSH remote-forwarding feature and consumer-facing SSH wrapping
remain supported in their existing mode. Consumer wrapping in Caddy is an
explicit opt-in TCP template; SSH remote-forward registration is still separate.
A future SSH registration adapter can implement the same stream-dialing
contract, but requires an explicit mapping from SSH forwarding identities to
bindings and real cancellation/deadline semantics.

## 6. HTTP: integrate with regular Caddy

Use the built-in `reverse_proxy` handler with a custom transport; do not build a
parallel HTTP router or replace Caddy's middleware chain. Caddy continues to own
Host/path/method/header matching, redirects, authentication middleware, header
processing, response handling, access logging, and frontend TLS.

Each transport configuration references one binding. The reverse proxy uses a
stable synthetic upstream address, `<binding>.revlb.invalid:80`, which is a pool
identity only. Binding names must be DNS-label-safe. The transport validates
that the requested upstream identity matches its configured binding and opens a
tunnel stream instead of resolving or dialing that address.

Synthetic identities MUST NEVER reach DNS, a forward proxy, or a normal network
dialer. Disable environment proxy lookup in the underlying transport. Do not
fall back to direct network access when a tunnel is unavailable.

The transport uses `net/http` and supported HTTP/2 transports with a tunnel
dialer. Reuse Caddy's public transport configuration/helpers where safe, but do
not assume its HTTP transport exposes a pluggable dialer for every path. In
particular, independently configured TLS and HTTP/2 dial paths must not bypass
the tunnel. Implement against public APIs rather than patching private fields.

Required behavior:

- HTTP/1.1 keep-alive, WebSocket upgrades, streaming responses/SSE, request
  cancellation, and Caddy's response-header and stream timeouts.
- HTTP/2 upstreams over TLS and explicit h2c for gRPC. TLS negotiation and
  certificate verification happen over the tunnel stream, independently of
  TLS protecting the tunnel control connection.
- Caddy's normal HTTP request/response semantics, including headers and trusted
  proxy handling. Do not blindly rewrite Host to the synthetic upstream name.
- A verified upstream TLS configuration with an explicit backend server name
  when TLS is enabled; never derive certificate identity from `.invalid`.
- No upstream HTTP/3, arbitrary CONNECT forwarding, or forward-proxy behavior
  in this transport. These are not implied by frontend HTTP/2 or HTTP/3 support.

Keep connection pools isolated by binding, runtime generation, and backend TLS/
HTTP settings. A pooled connection is already attached to one session: random
selection occurs on a new upstream connection, not every HTTP request. HTTP/2
multiplexing strengthens this affinity. Caddy's load-balancing policies over a
single logical upstream do not become per-session policies.

Before admitting a round trip, check that the binding remains authorized and
has eligible sessions. Registry removal/revocation must invalidate affected
idle connections, and removed sessions must not receive new HTTP/2 streams.
Track session ownership of pooled connections; use a tested drain/retirement
mechanism for HTTP/2 rather than assuming `CloseIdleConnections` alone prevents
reuse of an active multiplexed connection. Hard revocation closes associated
connections; graceful drain allows admitted requests to finish.

Connection-level `SourceAddress` metadata describes the origin that opened that
stream, not every request subsequently reusing it. HTTP client identity travels
through Caddy's normal per-request forwarding headers and trust policy; never
use the first request's connection metadata to authorize later requests.

Unavailable bindings return an explicit error mapped to HTTP 503; opening/
transport failures map to 502, and eligible upstream timeouts to 504 through
Caddy's error handling. Do not manufacture success responses inside
`RoundTrip`. Preserve Caddy's configured retry rules and error classification.
Retries are disabled by default, must be bounded, and must not replay requests
after transmission without normal HTTP replay-safety guarantees.

Passive health accounting describes the logical binding, not each client.
Session liveness is necessary but is not application health. Active HTTP probes,
if configured, traverse the same transport and probe the binding, not every
registered backend. Per-session application health and dynamic-upstream modules
are deferred; they are not prerequisites for dynamic tunnel membership.

## 7. TCP: reuse caddy-l4 routing

Use caddy-l4 directly rather than implementing an approximately compatible
rules engine. The tunnel handler is another destination in its existing route
chains, composable with its TLS, subroute, PROXY protocol, and other handlers.

The following rule capabilities are required through the pinned caddy-l4
dependency:

| Rule capability | Integration behavior |
|---|---|
| Ordered routes | Preserve caddy-l4 route/handler order and terminal handling |
| Matcher sets | AND within a set, OR across sets, and upstream `not` semantics |
| TLS | Match ClientHello SNI/ALPN; either pass encrypted bytes unchanged or terminate with the upstream TLS handler |
| HTTP detection | Match supported initial HTTP properties such as Host; this remains connection routing, not per-request routing |
| Network identity | Reuse remote/local IP/CIDR matchers with documented proxy trust boundaries |
| Protocol detection | Reuse TCP-compatible matchers such as SSH and supported byte/regexp matching |
| Subroutes | Reuse nested handler chains, including routing after TLS termination |
| Fallback | Explicit catch-all handling; no implicit empty-instance or cross-tenant fallback |

Do not promise every caddy-l4 protocol matcher works over a TCP-only tunnel.
UDP-only protocols remain unsupported. A connection routed using its first HTTP
Host is pinned for its lifetime; use the HTTP app for keep-alive connections with
multiple hosts, HTTP/2 streams, path routing, or HTTP middleware.

`layer4.handlers.goreverselb` references a binding and acts as a terminal handler.
It MUST forward through `layer4.Connection` so bytes buffered during matching
are replayed exactly once. It MUST NOT unwrap the raw socket and discard
prefetched data, repeat goreverselb's standalone sniffing, or consume a TLS
ClientHello before passthrough.

After selection, obtain a backend stream and proxy both directions with bounded
resources and preserved half-closes. Verify the actual yamux stream API: do not
assume that calling `Close` means the same thing as `CloseWrite`, or that it is
safe to close both directions after the first EOF. Introduce a protocol-aware
adapter if necessary. Finish the reverse copy before final teardown.

No eligible session while the listener still exists means close the frontend
connection and emit a structured error, not an HTTP response on an unknown
protocol. After dynamic endpoint removal, the port no longer accepts
connections. A failed terminal handler
does not fall through into another tenant's route. Retry another eligible
session only before application bytes have been forwarded, with a bounded
attempt count and deadline.

The optional `goreverselb_route` handler preserves access for `stdinproxy` and
legacy CONNECT users on explicitly configured listeners. It parses the custom
`PROXY->` prefix or a bounded CONNECT request, maps the requested instance to an
allowlisted binding, consumes only that prefix, and replays remaining bytes.
For CONNECT, send success only after a tunnel stream has opened; this cannot
guarantee the client's local target connected under the existing protocol.
Unknown instances, incomplete prefixes, oversized requests, and timeouts must
fail explicitly. `PROXY->` is not HAProxy PROXY protocol v1/v2 and must not be
handled by that parser.

## 8. Listener and TLS ownership

The app owns a dedicated control listener for client TLS + yamux connections.
Provision its server TLS configuration through Caddy's TLS facilities and
configured certificate policies; do not generate an ephemeral certificate per
app load. Support operator-supplied certificates and explicit Caddy-managed
names. Certificate automation eligibility comes from operator configuration,
never an unauthenticated tunnel registration.

Frontend TLS has three distinct modes:

| Mode | TLS endpoint |
|---|---|
| Normal HTTP/HTTPS | Caddy HTTP app terminates TLS and proxies HTTP over the tunnel |
| Raw TCP TLS passthrough | Private backend terminates TLS; L4 forwards ClientHello unchanged |
| Raw TCP TLS termination | caddy-l4 TLS handler terminates TLS before the tunnel handler |

Control-plane TLS and backend HTTP TLS are independent of these modes.
Existing clients currently skip server certificate verification regardless of
the tunnel flag. The integration must not claim otherwise. Add an explicit,
end-to-end client trust configuration for verified Caddy deployments, retaining
the old behavior only as a documented standalone compatibility option during
migration. Cover tunnel and tunnelgroup clients and avoid silently breaking
existing self-signed deployments.

Initially demonstrate distinct HTTP, L4, and control sockets. For shared
TCP `:443`, use caddy-l4's existing listener wrapper on the HTTP server, ahead of
the TLS wrapper: explicitly matched passthrough connections go to tunnels;
unmatched connections return to the normal Caddy TLS/HTTP stack. Do not bind
independent HTTP and L4 servers to the same address or create a second custom
socket demultiplexer.

Shared-port configurations must preserve HTTP certificate challenges and HTTP/2
negotiation. TCP wrappers do not intercept HTTP/3/UDP. Control traffic remains
on its dedicated port in the first release; existing clients are not assumed to
send a distinguishing ALPN token.

## 9. Proposed configuration

### 9.1 Primary example: dynamic endpoints from templates

This is proposed input configuration. No endpoint, service binding, frontend
port, HTTP server, or L4 server is predeclared. The example authorizes the user
to register new services in the configured pool; `web-*` services select the
HTTP template, and other services select the default TCP template.

```json
{
  "admin": {"listen": "localhost:2019"},
  "apps": {
    "goreverselb": {
      "runtime_id": "edge",
      "control": {
        "listen": ["tcp/:9999"],
        "tls": {"server_name": "tunnels.example.com"}
      },
      "users": {
        "default@none": {
          "token_env": "REVLB_TOKEN",
          "registration": {"service_patterns": ["*"]}
        }
      },
      "publication": {
        "mode": "dynamic",
        "admin_endpoint": "http://localhost:2019",
        "bind_host": "0.0.0.0",
        "advertise_host": "tunnels.example.com",
        "port_start": 8000,
        "port_count": 100,
        "default_template": "raw-tcp",
        "rules": [
          {
            "user_id": "default@none",
            "service_pattern": "web-*",
            "template": "plain-http"
          }
        ],
        "templates": {
          "raw-tcp": {
            "kind": "tcp",
            "instance_dispatch": "direct"
          },
          "plain-http": {
            "kind": "http",
            "instance_dispatch": "direct",
            "frontend_tls": false
          }
        }
      }
    }
  }
}
```

`service_patterns` and `service_pattern` use documented, anchored glob matching
over the validated service name, not an expression evaluator. Rules are ordered,
with first match winning; the default template still requires registration
authorization. Production users should receive suitably scoped patterns and
quotas. The control TLS certificate must be supplied/managed using an issuer
whose challenge mechanism is reachable; this example does not assume an HTTP
listener on port 80 exists for ACME.

Existing clients can request automatic or specific ports:

```sh
# Automatically allocate a TCP frontend, for example port 8000.
./goreverselb tunnel -e tunnels.example.com:9999 \
  -s shell -b 127.0.0.1:22

# Publish a regular Caddy HTTP listener on exactly port 8005.
./goreverselb tunnel -e tunnels.example.com:9999 \
  -s web-demo -p 8005 -b 127.0.0.1:8080
```

These commands assume `REVLB_TOKEN` is set and demonstrate the existing wire
format, not verified TLS with the current client. If 8005 is unavailable, the
second registration fails. Neither operation requires a Caddyfile edit.

### 9.2 Inspect the generated configuration

After those registrations commit, the standard Caddy admin API must expose the
materialized listeners. These are illustrative excerpts; actual endpoint IDs
are controller-assigned:

```sh
curl http://localhost:2019/config/apps/layer4/servers
curl http://localhost:2019/config/apps/http/servers
curl http://localhost:2019/config/apps/goreverselb/generated
curl http://localhost:2019/id/revlb-edge-e42-server
```

Example `apps.layer4.servers` result for the TCP registration:

```json
{
  "revlb-edge-e42": {
    "@id": "revlb-edge-e42-server",
    "listen": ["tcp/0.0.0.0:8000"],
    "routes": [{
      "handle": [{"handler": "goreverselb", "binding": "b-e42"}]
    }]
  }
}
```

Example `apps.http.servers` result for the HTTP registration:

```json
{
  "revlb-edge-e43": {
    "@id": "revlb-edge-e43-server",
    "listen": ["0.0.0.0:8005"],
    "automatic_https": {"disable": true},
    "routes": [{
      "handle": [{
        "handler": "reverse_proxy",
        "upstreams": [{"dial": "b-e43.revlb.invalid:80"}],
        "transport": {"protocol": "goreverselb", "binding": "b-e43"}
      }]
    }]
  }
}
```

The same transaction creates manifest entries associating `b-e42` with
`(default@none, shell, "")` and `b-e43` with `(default@none, web-demo, "")`,
including the ports, template revisions, and generated IDs. These bindings are
not a prerequisite in the input configuration; they are controller output.
An HTTP request to `http://tunnels.example.com:8005/` passes through Caddy's HTTP
handler chain into the registered tunnel. When the last session for `shell`
leaves and removal completes, the e42 server and manifest entry disappear from
these queries and port 8000 is returned to the pool.

### 9.3 Dynamic Caddyfile surface

Equivalent proposed policy/template syntax:

```caddyfile
{
    admin localhost:2019
    goreverselb {
        runtime_id edge
        control :9999 {
            tls tunnels.example.com
        }
        user default@none {
            token_env REVLB_TOKEN
            register_services *
        }
        publication dynamic {
            admin_endpoint http://localhost:2019
            bind_host 0.0.0.0
            advertise_host tunnels.example.com
            ports 8000 100
            default_template raw-tcp
            rule default@none web-* plain-http
            template raw-tcp {
                kind tcp
                instance_dispatch direct
            }
            template plain-http {
                kind http
                instance_dispatch direct
                frontend_tls off
            }
        }
    }
}
```

Here `ports` takes a starting port and count, not two inclusive endpoints.
Adaptation emits only the base policy. Subsequent generated routes are stored
in Caddy's active JSON/autosave, not written back to this source Caddyfile.

#### Certificate files without static sites

The implemented extended adapter, selected with
`--adapter goreverselb-caddyfile`, accepts repeatable
`certificate <certificate-chain.pem> <private-key.pem>` directives inside the
global `goreverselb` block. It delegates ordinary syntax, imports, environment
substitution, regular sites, and warnings to Caddy's standard adapter, then
merges those file pairs into `apps.tls.certificates.load_files`.

The resulting native JSON must contain only standard TLS loader configuration,
not adaptation-only fields on the goreverselb app. Preserve unrelated apps,
TLS automation/PKI policies, other certificate loaders, and existing file
loader tags. Deduplicate identical file pairs without removing site-specific
certificate selection. Adding certificate files must not create a static
listener or take ownership of a dynamically allocated port.

Use Caddy's existing loader for file access, key/certificate validation,
certificate caching and cleanup. Never embed PEM/key contents in the generated
configuration or add a parallel certificate cache. File load failures reject
provisioning; rejected reloads preserve the active configuration. Externally
renewed files are re-read on forced reload. When a source-policy reload omits
generated routes, reconciliation must restore still-authorized live bindings
from recorded publication leases, retaining acknowledged ports rather than
allocating replacements. Missing leases or incompatible publication policy must
fail explicitly. Current restoration is asynchronous and may briefly interrupt
new frontend connections. Ordinary Caddyfiles without this
directive retain standard adapter behavior; using the directive without the
extended adapter must produce an actionable error rather than silently ignore
it. The checked-in `caddy/examples/certificates.Caddyfile` demonstrates a
supplied wildcard certificate, control port 9000 and requested frontend 7445.

### 9.4 Optional static bindings: HTTP and L4 using the same registry

This is the target schema, not a configuration accepted by today's repository.
`token_env` resolves an environment variable at provisioning without persisting
the expanded secret in the module's serialized configuration. The two configured
bindings also form the registration allowlist for this explicitly selected
`bindings_only` example. This is an alternative to dynamic publication, not
required setup for it.

```json
{
  "apps": {
    "goreverselb": {
      "runtime_id": "edge",
      "control": {
        "listen": ["tcp/:9999"],
        "tls": {"server_name": "tunnels.example.com"}
      },
      "publication": {"mode": "bindings_only"},
      "users": {
        "default@none": {"token_env": "REVLB_TOKEN"}
      },
      "bindings": {
        "web": {
          "user_id": "default@none",
          "service": "web",
          "instance": ""
        },
        "ssh": {
          "user_id": "default@none",
          "service": "ssh",
          "instance": "node-a"
        }
      }
    },
    "http": {
      "servers": {
        "public": {
          "listen": [":443"],
          "routes": [{
            "match": [{"host": ["app.example.com"]}],
            "handle": [{
              "handler": "reverse_proxy",
              "upstreams": [{"dial": "web.revlb.invalid:80"}],
              "transport": {
                "protocol": "goreverselb",
                "binding": "web"
              }
            }]
          }]
        }
      }
    },
    "layer4": {
      "servers": {
        "ssh": {
          "listen": ["tcp/:2222"],
          "routes": [{
            "handle": [{"handler": "goreverselb", "binding": "ssh"}]
          }]
        }
      }
    },
    "tls": {
      "automation": {
        "policies": [{
          "subjects": ["app.example.com", "tunnels.example.com"]
        }]
      }
    }
  }
}
```

The app must explicitly request management of its configured control certificate
name; declaring an automation policy alone is not sufficient to initiate
certificate management. ACME challenge reachability or an alternative issuer
must be configured for the deployment.

### 9.5 Static-binding Caddyfile surface

The new global `goreverselb` option configures the app. The `goreverselb`
transport subdirective integrates with the existing `reverse_proxy` directive.
The L4 `goreverselb` directive is registered only in the L4 handler context.

```caddyfile
{
    goreverselb {
        runtime_id edge
        control :9999 {
            tls tunnels.example.com
        }
        publication bindings_only
        user default@none {
            token_env REVLB_TOKEN
        }
        binding web {
            user_id default@none
            service web
        }
        binding ssh {
            user_id default@none
            service ssh
            instance node-a
        }
    }

    layer4 {
        :2222 {
            route {
                goreverselb ssh
            }
        }
    }
}

app.example.com {
    reverse_proxy web.revlb.invalid:80 {
        transport goreverselb {
            binding web
        }
    }
}
```

Existing client commands for the bindings above:

```sh
REVLB_TOKEN="$REVLB_TOKEN" ./goreverselb tunnel \
  -e tunnels.example.com:9999 -s web -b 127.0.0.1:8080

REVLB_TOKEN="$REVLB_TOKEN" ./goreverselb tunnel \
  -e tunnels.example.com:9999 -s ssh \
  --instancename node-a -b 127.0.0.1:22
```

These examples use the existing environment variable and default port `0`.
They demonstrate wire compatibility, not verified TLS with the current client.

### 9.6 L4 rule composition

Given additional allowlisted bindings `database` and `secure-shell`, use the
existing caddy-l4 matcher syntax:

```caddyfile
layer4 {
    :8443 {
        @database tls sni db.example.com
        route @database {
            goreverselb database
        }

        @shell {
            remote_ip 10.0.0.0/8
            tls sni ssh.example.com
        }
        route @shell {
            tls
            goreverselb secure-shell
        }
    }
}
```

This is a fragment of the global options block. The first route passes TLS
through; the second terminates TLS using an explicitly configured certificate
policy. Unmatched traffic on this dedicated L4 listener closes. A server-first
protocol such as SSH should normally use a dedicated catch-all listener as in
the complete example, rather than waiting for a client protocol banner.

All examples must become executable configuration fixtures. JSON is the
canonical schema; Caddyfile adaptation must produce equivalent module objects
and must reject unknown fields, missing bindings, invalid selectors, unsupported
protocols, and conflicting listener ownership. Dynamic input without generated
bindings is valid; generated candidates must contain every referenced binding.
The L4 composition example can also be emitted by an authorized template rather
than supplied as static configuration.

## 10. Lifecycle, reload, and resource policy

Provisioning parses configuration, resolves references, prepares TLS/auth
resources, and validates invariants. It must not start accept loops, allocate
dynamic frontend ports, or write standalone state. `Start` activates the runtime;
`Stop`/`Cleanup` are idempotent and release only resources owned by that instance.
HTTP/L4 provisioning may request the app through `ctx.App`, but must not assume
its listener has already started. Avoid circular app dependencies. Readiness
for registration additionally requires the controller to observe that its
configuration generation is committed; starting an app during a candidate
load is not proof that the whole config will succeed.

Caddy starts new configurations before stopping old ones. Use Caddy listener
sharing facilities and a reference-counted runtime (for example,
`caddy.UsagePool`) rather than uncoordinated package-global session maps.

The reload contract is:

1. Route-only changes with an unchanged runtime identity and control/security
   configuration reuse live sessions and do not disconnect clients. This
   includes controller-generated endpoint additions and removals: frequent
   publications must not restart the control listener's accepted sessions.
2. A new configuration acquires references during preparation; failed validation
   or startup releases those references without damaging the active runtime.
3. Authorization/binding changes are applied only at successful activation.
   Removed permissions deny new streams immediately. Credential revocation
   disconnects sessions authenticated by revoked credentials and invalidates
   affected HTTP pools.
4. Incompatible listener/transport changes create a new generation and retire
   the old one. Old sessions are not silently adopted into a different tenant or
   trust policy. Clients reconnect through their existing retry behavior.
5. Graceful shutdown stops accepting registrations/new streams, drains admitted
   traffic for a configured deadline, then closes remaining streams, sessions,
   listeners, and registry references.

Shared listening sockets alone do not preserve accepted yamux sessions; the
registry/session owner must survive compatible reloads too. Retired generations
must not race with a replacement to delete new registrations or return ports
they no longer own. Explicitly test rollback, replacement, and repeated cleanup.

The shared controller must not be tied to a short-lived app context that its own
config commit cancels. Lifecycle cleanup releases references without waiting
for in-flight admin transactions; final runtime shutdown cancels/drains those
operations outside Caddy's configuration lock. Only the committed generation
may initiate new transactions. Failed candidates cannot change the active
policy, release its ports, or retire its sessions.

### 10.1 Endpoint removal and port reuse

Leases transition through `pending -> active -> draining -> removed`, with
explicit `failed` and `reconciling` states. On last-session loss, stop admission
immediately and enqueue removal; default reconnect grace is zero, with an
optional bounded grace period to reduce churn. Within a configured grace period,
HTTP returns 503 and TCP closes if no eligible session exists.

Remove generated listener/routes, orphaned bindings, and the manifest entry in
one conditional config transaction. Delete only the departing instance's route
if other instances still own the endpoint. Removing a shared static binding's
last session must not delete an operator-owned listener.

Do not release a port until removal is confirmed and the retired listener has
stopped accepting; existing admitted streams follow drain policy. If removal
fails or the admin endpoint is unavailable, quarantine the reservation, expose
the reconciliation error, and retry with bounded backoff. Never allocate the
same port to another service while an old generated listener may still own it.

A reconnect before removal commits cancels removal or joins the current
operation by lease generation. If the delete was already committed, perform a
new publication transaction; honor a reconnecting client's nonzero remembered
port only if it can be reacquired. Stale cleanup cannot delete its replacement.

### 10.2 Persistence, restart, and operator changes

The base Caddyfile/JSON is operator policy. The active Caddy JSON is that policy
plus generated publication objects; Caddy normally autosaves the latter.
Configuration durability does not preserve TCP/yamux sessions across process
restart, and Caddy autosave can be disabled or fail independently of activation.
Do not equate an admin success response with guaranteed disk durability.

Keep a crash-recoverable, versioned lease/ownership journal using configured
Caddy storage, not working-directory state. Record intent before submission
and reconcile it with observed native config after submission. Never store
credentials or live connection state there. The journal is not a replacement
for config visibility or Caddy's authoritative committed revision.

On `--resume`, rehydrate reservations from the journal and generated manifest
before accepting new registrations. Restored bindings without authenticated
sessions remain unavailable. Give previous clients a configurable restart
grace period, then remove orphaned generated config transactionally. A listener
that was restored does not itself prove a tunnel is ready.

On loading only the base config, reconcile authenticated live registrations
against the newly committed policy before regenerating missing owned objects.
After a process restart there are no live registrations: journal records alone
must not republish old services. Reconnecting authorized clients may recover
uncontested leases. A missing/replaced runtime or revoked publication permission
prevents regeneration, even if the journal contains old endpoints.

Template updates are versioned and reconcile affected live endpoints
transactionally. An incompatible update fails visibly instead of silently
changing assigned ports or downgrading wrapping. Endpoint-affecting policy
revocation denies admission immediately and schedules cleanup; unrelated
operator edits are preserved through ETag rebase. Conflicting direct edits of
generated objects follow the ownership-conflict rule in section 5.4.

### 10.3 Limits

Initial configurable limits and defaults:

| Setting | Initial default |
|---|---|
| Control TLS handshake / registration timeout | 10s each |
| Stream open plus metadata-write timeout | 10s total |
| L4 matching timeout | Pinned caddy-l4 default, initially 3s |
| Legacy preamble inspection | 4 KiB maximum, 3s timeout |
| Control registration JSON | 64 KiB maximum |
| Connection metadata frame | 1000 bytes maximum |
| Sessions | 1024 total, 64 per user |
| Concurrent admitted streams | 4096 total, 1024 per user |
| Published/pending endpoints | Bounded by configured pool and per-user quota |
| Pending publication operations | 256 per runtime |
| Config publication deadline | 15s, excluding later reconciliation of unknown outcomes |
| Disconnect reconnect grace | 0s |
| Restart recovery grace | 30s |
| Graceful drain | 30s |

These are proposed operational defaults, not measured capacity claims. Validate
positive bounds, enforce pending-connection/open limits as well as admitted
streams, and make limit failures observable. Do not queue an unbounded number
of requests waiting for a client to register. Distinguish the timeout for reading
registration input from the publication deadline after the input is received.
If the controller cannot establish success before that deadline, do not return
a successful endpoint; retain/quarantine unresolved leases for reconciliation.

## 11. Authorization and observability

Tunnel registration credentials are not HTTP consumer authentication. Require
both credential authorization and operator permission for the requested
service/instance; separately use normal HTTP middleware or L4 network rules to
authorize frontend consumers. A registered client cannot expand its own
permissions through `AllowedSources` or requested frontend settings.

Preserve existing valid user/service token precedence in the compatibility
adapter. In Caddy mode, missing credentials or unresolved secret references
fail provisioning, and identities/services outside registration policy fail
closed. A new service allowed by a user's registration policy does not require a
predeclared binding. Do not import the standalone default-token overwrite
behavior. Set per-user publication quotas as well as session/stream quotas so
one user cannot consume the entire port pool or trigger unlimited Caddy reloads.

The persisted `AllowedSources` field is not currently enforced by the ordinary
yamux proxy. The integration must not present it as protection. Configure and
enforce Caddy consumer policy explicitly. Trust forwarded source addresses only
from configured trusted proxies; SNI, Host, CONNECT targets, and custom instance
names are routing inputs, never credentials.

Log through Caddy's structured logger with runtime, user, binding, instance,
session ID, error category, and connection/request correlation where available.
Never log tokens, complete registration structs, request bodies, or expanded
secret values. Session IDs must be opaque and must not be raw credentials.

Expose bounded-cardinality counters/gauges for active sessions and streams,
registration failures, unavailable bindings, opens/timeouts, bytes transferred,
revocations, allocated/quarantined ports, pending publications, config commit
latency/failures, ETag conflicts, and drain outcomes through the supported Caddy
metrics mechanism.
Keep normal HTTP access logs intact. No unauthenticated copy of the standalone
REST status service is started. Caddy's own admin endpoint remains subject to
the deployment's normal access controls.

## 12. Delivery plan and acceptance criteria

### Phase 1: extract and preserve the standalone core

Separate the registry and stream dialer from listeners, retain public wrappers,
and add deterministic framing, authorization, lifecycle, and half-close tests.
Keep current CLI defaults and service-group behavior. Fix defects encountered
at these integration boundaries, rather than copying them into the module.

### Phase 2: dynamic Caddy publication, HTTP transport, and TCP handler

Ship the importable nested module, dynamic publication controller, automatic
and explicit port allocation, TCP/HTTP templates, atomic native config updates,
admin visibility, native JSON/Caddyfile support, standard HTTP reverse proxy
transport, caddy-l4 handler, and explicitly assembled `cmd/caddy-reverselb`
application. Endpoint lifecycle and self-reload safety are
prerequisites, not deferred polish. Both HTTP and TCP are required for this
milestone; static routes or a TCP-only plugin do not complete this spec.

### Phase 3: compatibility and operational completion

Complete optional static bindings, opt-in legacy preamble routing, verified-client
TLS migration, shared-port fixtures, recovery/conflict handling, limits, metrics,
and operator documentation. SSH-backend-to-Caddy bindings remain a separate
future feature; standalone SSH support must remain functional throughout.

Release acceptance requires:

| Area | Observable requirement |
|---|---|
| Packaging | A clean external `xcaddy` build imports the published module without local replacements; expected module IDs appear |
| Explicit application | With `xcaddy` absent, root `go build -v ./cmd/caddy-reverselb` and independent `GOWORK=off go build .` in that directory produce the full Caddy CLI; `list-modules` includes standard HTTP/TLS, `layer4`, and goreverselb modules |
| Application parity | The repository binary adapts/validates the examples and serves dynamic HTTP/TCP endpoints visible through its normal admin API |
| Standalone | Existing root CLI and library builds remain Caddy-independent; tunnel, tunnelgroup, stdinproxy, wrapping, and SSH reverse forwarding regressions pass |
| Config | Every documented JSON/Caddyfile fixture adapts and validates against pinned dependencies |
| Dynamic publication | With only policy/templates configured, port `0` creates a listening Caddy endpoint; an explicit request gets exactly that port or fails; response follows activation |
| Admin visibility | `GET /config/`, per-app queries, and `/id/` show generated native HTTP/L4 objects and manifest; cleanup removes them |
| Dynamic membership | Additional sessions reuse a published endpoint without a config transaction; last-session loss removes dynamic config and releases its port after confirmed cleanup |
| Templates | Authorized new services need no predeclared binding; TCP/HTTP templates preserve requested routing/wrapping policy; invalid requests fail |
| Isolation | Identical service/instance names in different users never cross-route; unauthorized registration and unknown instance input fail closed |
| HTTP | Host/path routing, middleware, headers, keep-alive, WebSocket, SSE, cancellation, verified upstream TLS, and gRPC over HTTP/2/h2c work through real tunnels |
| No bypass | Instrumented dial/DNS tests prove synthetic upstreams and failures never escape to ordinary sockets or environment proxies |
| L4 | SNI passthrough preserves the original handshake; TLS termination forwards plaintext; matcher order, IP restrictions, and buffered-byte replay work |
| Legacy inputs | Fragmented custom prefixes and CONNECT requests, surplus bytes, invalid lengths, and timeouts are handled deterministically |
| Half-close | A loopback client sends a request, closes its write side, and receives a complete delayed response over both standalone and Caddy tunnel paths |
| Port lifecycle | Bind failures roll back, conflicts fail clearly, final removal releases exactly one reservation, and ports can be reused |
| Transactions | Concurrent registrations/admin edits preserve unrelated config; 412 causes rebase; bind failures leave old config intact; lost replies reconcile without double allocation |
| Reload | Repeated compatible reloads preserve an established tunnel and long-lived transfer; failed reloads preserve the active config; incompatible changes drain within policy |
| Controller lifecycle | Self-generated reloads neither deadlock nor restart registration sessions, duplicate controllers, loop endlessly, or acknowledge uncommitted endpoints |
| Recovery | Resume/base-config restart, orphan cleanup, client reconnect, journal/config disagreement, and failed deletions cannot publish stale backends or reuse quarantined ports |
| Revocation | New HTTP requests/HTTP2 streams and TCP connections cannot reuse revoked sessions or pooled connections |
| Limits | Oversized registration/frame/preamble and excessive sessions/streams are rejected at their configured thresholds without leaked resources |
| Shutdown | All owned listeners, goroutines, streams, sessions, and references terminate after the drain deadline |

Use real loopback Caddy/server/client processes for lifecycle and protocol tests,
including older client binaries where compatibility is claimed. Unit tests
alone cannot verify yamux ordering, HTTP pooling, or listener handoff.

Run root formatting, `go test -vet=off ./...`, `go build ./cmd/goreverselb`, and
`go vet ./...`, distinguishing known standalone vet findings from regressions.
Run tests/build/vet independently in both nested modules, `caddy` and
`cmd/caddy-reverselb`; root `./...` does not cover them. CI must build the
repository application with plain Go, without installing or invoking `xcaddy`,
and exercise its CLI and end-to-end fixtures. Add race-enabled tests for registry membership,
pool retirement, reload, and concurrent stream open/close.

Update `README.md` and release documentation with installation, supported
versions, complete dynamic HTTP/TCP examples, template policies, admin queries,
trust configuration, dynamic versus bindings-only publication, limitations of
old clients, and migration/rollback steps. Document the plain-Go
`cmd/caddy-reverselb` build separately from third-party `xcaddy` integration and
the unchanged standalone CLI.
No performance improvement is claimed until measured against equivalent
standalone and Caddy loopback-proxy baselines.

## 13. Design alternatives

**Proxy Caddy to dynamically allocated localhost ports:** simpler initial wiring,
but retains port allocation, extra sockets, frontend sniffing, and lifecycle
coupling. Not the primary architecture.

**Implement a new HTTP handler/router:** duplicates Caddy reverse proxy behavior
and risks losing middleware, streaming, and protocol support. Use a transport.

**Implement a caddy-l4-like app from scratch:** introduces a second rules engine
with subtly different matcher and buffering behavior. Extend caddy-l4 instead.

**Publish each session through an HTTP dynamic-upstream module:** potentially
useful for per-session Caddy load balancing later, but requires stable session
addresses, pool retirement, and health semantics. A binding-level transport
already provides dynamic tunnel availability without this extra public API.

**Give clients direct Caddy admin access or accept client-supplied module JSON:**
confuses registration authority with server administration. Instead, clients
request endpoints through the existing control plane; the trusted controller
renders authorized templates and commits native configuration on their behalf.

**Create listeners only inside goreverselb and add a custom inventory API:**
does not meet the requirement that generated configuration be queryable through
Caddy's standard control plane. Materialize real HTTP/L4 objects under
`/config/`; custom status reporting is only supplemental.

## 14. References

The design is based on the repository files named above and the following
upstream documentation/source. Upstream default branches are moving references;
implementation must pin and reverify its supported versions.

- [Caddy repository](https://github.com/caddyserver/caddy)
- [Extending Caddy: modules and lifecycle](https://caddyserver.com/docs/extending-caddy)
- [Caddy admin API: config transactions, IDs, and ETag concurrency](https://caddyserver.com/docs/api)
- [Caddy configuration load and lifecycle implementation](https://github.com/caddyserver/caddy/blob/master/caddy.go)
- [Caddy's explicit application entry point](https://github.com/caddyserver/caddy/blob/master/cmd/caddy/main.go)
- [Caddy reverse_proxy](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy)
- [Caddy HTTP transport source](https://github.com/caddyserver/caddy/blob/master/modules/caddyhttp/reverseproxy/httptransport.go)
- [caddy-l4 project and build instructions](https://github.com/mholt/caddy-l4)
- [caddy-l4 routes](https://github.com/mholt/caddy-l4/blob/master/docs/routes.md)
- [caddy-l4 matchers](https://github.com/mholt/caddy-l4/blob/master/docs/matchers.md)
- [caddy-l4 servers and listener wrappers](https://github.com/mholt/caddy-l4/blob/master/docs/servers.md)
- [caddy-l4 handler interfaces](https://github.com/mholt/caddy-l4/blob/master/layer4/handlers.go)
