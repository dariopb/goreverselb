# SSH reverse-backend protocol specification

## Status

Proposed specification for adding standard SSH remote forwarding as a second backend transport. The existing TLS/yamux backend protocol remains supported and unchanged.

The first version deliberately supports only the default user and port-based services. Named services, instance routing, public-key authentication, and SSH frontend wrapping are outside its scope.

## Goals

- Let a backend operator publish a local TCP service using an ordinary OpenSSH-compatible client and `ssh -R`.
- Authenticate the SSH connection with the same token the server already validates for the default goreverselb user.
- Reuse goreverselb's frontend port pool, listener lifecycle, random backend selection, and bidirectional proxy behavior.
- Allow multiple SSH clients to register the same frontend port and load-balance incoming connections across them.
- Run alongside, without changing, the existing TLS/yamux backend protocol.

## Non-goals for the first version

- Replacing or tunneling the existing yamux protocol over SSH.
- Supporting `ssh -L`; local forwarding consumes a service and does not register a backend.
- Supporting shell, command, PTY, agent, X11, Unix-socket, or dynamic SOCKS forwarding.
- Supporting SSH public-key authentication.
- Selecting named users from `user_store.yaml`.
- Supplying goreverselb service names or instance names through SSH.
- Mixing SSH and yamux backends in one logical service.
- Supporting `--wrapSSH` on the resulting frontend. `--wrapSSH` is an independent consumer-facing SSH wrapper and must remain separate from this backend-facing SSH server.

## Server configuration

Add a dedicated SSH backend listener. It must not share the TLS/yamux API port because SSH and TLS have different handshakes and protocol lifecycles.

Proposed server options:

```text
--sshbackendport <port>
REVLB_SSH_BACKEND_PORT=<port>

--sshbackenduser <username>
REVLB_SSH_BACKEND_USER=<username>
```

A backend port value of `0` disables the SSH backend listener. The listener binds on all interfaces, matching the existing tunnel and frontend listeners. The backend username defaults to `reverseuser`; startup must reject an empty username.

The SSH backend listener must use the persistent `revlb_ssh_host_key` already created by `loadOrCreateSSHKey`. It must not generate a second host identity. Operators are responsible for distributing or verifying that host key through normal SSH mechanisms.

## Client interface

### Port supplied by `-R`

The configured SSH backend username selects the default goreverselb user. It defaults to `reverseuser`. The requested remote forwarding port becomes the public frontend port:

```bash
ssh -N -R 8005:127.0.0.1:22 reverseuser@SERVER -p SSH_BACKEND_PORT
```

This publishes the client's `127.0.0.1:22` through `SERVER:8005`.

A requested remote port of `0` asks the server to allocate a port from its dynamic frontend pool using standard SSH allocated-port reply semantics:

```bash
ssh -N -R 0:127.0.0.1:22 reverseuser@SERVER -p SSH_BACKEND_PORT
```

OpenSSH can print the allocated port when verbose logging is enabled. Programmatic clients receive it in the successful `tcpip-forward` reply.

The first successful `tcpip-forward` request selects the connection's effective frontend port. Later requests on that connection may repeat that port but may not select another port. Use a separate SSH connection for each frontend port.

Only the exact configured SSH backend username is accepted. It authenticates as `DefaultUserID` (`default@none`). The server must terminate the SSH transport immediately when the username differs, without allowing password retries or processing channels/global requests.

The password prompt must be answered with the goreverselb token:

```text
reverseuser@SERVER's password: <token>
```

Clients may use `sshpass`, `SSH_ASKPASS`, or another SSH library when unattended password entry is required. The specification does not prescribe secret-delivery tooling.

## Authentication

Only SSH password authentication is accepted in the first version.

1. Compare the presented SSH username with the configured backend username.
2. If it differs, terminate the SSH connection immediately. Do not continue authentication or offer another attempt.
3. Resolve the accepted username to `DefaultUserID`.
4. Load that user's token from the in-memory `ConfigData.Users` populated from `user_store.yaml`.
5. Compare the supplied SSH password with that token.
6. Accept the SSH connection only on an exact match.

The server command currently overwrites the default user's persisted token with the global `--token`/`REVLB_TOKEN` value at startup. Therefore that global token is the expected SSH password under the current configuration flow.

Authentication requirements:

- Compare tokens in constant time.
- Never log the password/token or include it in errors.
- Log only the remote address, username match/mismatch, and authentication success/failure at an appropriate level.
- Reject keyboard-interactive, public-key, `none`, and host-based authentication in the first version.
- Apply an SSH handshake deadline so unauthenticated connections cannot remain open indefinitely.

This authentication policy is stricter than the existing consumer-facing `--wrapSSH` implementation, which currently accepts every password. The two handlers must not share that permissive password callback.

## SSH protocol behavior

Implement standard SSH TCP/IP remote forwarding as defined by RFC 4254.

### Allowed global requests

The server accepts only:

- `tcpip-forward`
- `cancel-tcpip-forward`
- `keepalive@openssh.com`, replying success when requested

Unknown global requests receive a failure reply when one was requested.

The `tcpip-forward` payload contains:

```text
string  bind_address
uint32  bind_port
```

The server validates or resolves `bind_port`, registers the SSH connection as a backend, and replies success. When the client requested port `0`, the success payload contains the allocated `uint32` port.

The `cancel-tcpip-forward` payload uses the same fields. It removes only the registration belonging to that SSH connection. Cancellation is idempotent from the server's lifecycle perspective; a request for a registration not owned by that connection receives failure.

### Bind-address policy

The first version treats the requested SSH bind address as advisory. Every accepted remote forward is exposed using goreverselb's existing public frontend behavior (`0.0.0.0:<port>`).

Accept these common values:

- empty string
- `localhost`
- `127.0.0.1`
- `::1`
- `0.0.0.0`
- `::`
- `*`

Do not create separate listeners for different bind-address strings on the same port. Document to users that OpenSSH's `GatewayPorts` distinction does not apply: goreverselb frontends are public listeners unless server-wide binding configuration is added later.

### Disallowed channels and requests

Reject client-opened channels, including:

- `session`
- `direct-tcpip`
- `x11`
- `auth-agent@openssh.com`

Reject PTY, command execution, environment mutation, subsystem, and shell requests. A client using `-N` should not request any of them.

## Registration model

Each accepted remote forward creates an SSH backend registration with at least:

```text
connection identity
SSH server connection
frontend port
requested bind address
registration time
closed state
```

The logical identity for the first version is:

```text
user = DefaultUserID
service = ssh-reverse:<frontend-port>
instance = empty
transport = ssh
```

This synthetic service name is internal and must not be sent to the SSH client. It prevents accidental collision with arbitrary named yamux services.

The runtime frontend abstraction must be generalized so a frontend can select a backend transport without assuming every backend is a `*yamux.Session`. A backend interface should provide operations equivalent to:

```text
OpenConnection(source address) -> bidirectional stream
IsClosed() -> bool
Identity() -> stable connection identifier
```

The existing yamux implementation opens a yamux stream and sends `TunnelConnecData`. The SSH implementation opens a `forwarded-tcpip` channel. Frontend listener ownership, random selection, removal, and port return should be shared transport-independent logic rather than duplicated.

### Port allocation and collisions

- Port `0` uses the existing configured dynamic range `[dynport, dynport+dynportcount)`.
- An explicit port must be available from the same `PoolInts` range, matching current yamux behavior.
- The first registration for an SSH service acquires the port and creates its frontend listener.
- Additional SSH registrations for the same frontend port join the existing SSH service without acquiring or binding the port again.
- An SSH registration cannot join a port owned by a yamux service in the first version; reject it with SSH request failure.
- A yamux registration cannot take a port owned by an SSH reverse service; preserve the existing allocation failure behavior.
- The port is returned to the pool only after the last SSH registration is removed and the frontend listener has stopped.

All registration, selection, cancellation, disconnect cleanup, listener closure, and pool-return transitions must be serialized under the service's existing synchronization strategy. Network accepts, SSH channel opens, and byte copies must occur outside global/service mutexes.

## Incoming frontend connection flow

For each TCP connection accepted on an SSH reverse frontend:

1. Snapshot the active SSH backend registrations for that port.
2. Randomly select one registration, matching current yamux session load balancing.
3. Open a `forwarded-tcpip` channel on the selected SSH connection.
4. Proxy the frontend connection and channel in both directions.
5. Preserve half-close semantics where the SSH channel implementation supports `CloseWrite`.

The `forwarded-tcpip` channel-open payload is:

```text
string  connected_address
uint32  connected_port
string  originator_address
uint32  originator_port
```

Populate it as follows:

- `connected_address`: the normalized frontend bind address, preferably `0.0.0.0` for the first version.
- `connected_port`: the public frontend port.
- `originator_address`: the frontend TCP peer IP.
- `originator_port`: the frontend TCP peer port.

The standard SSH client uses the local destination from its `-R` command to connect to the backend, such as `127.0.0.1:22`. That destination is client-side state and is not transmitted in `tcpip-forward`; goreverselb neither needs nor receives it.

If opening the selected SSH channel fails before frontend bytes are forwarded, mark a closed connection unavailable and try each remaining active registration at most once in randomized order. Close the frontend connection when no backend accepts the channel. Do not retry after application bytes may have been delivered because doing so could duplicate a partial request.

## Load balancing

SSH provides multiplexing without yamux: each incoming frontend connection is a separate `forwarded-tcpip` SSH channel on the long-lived SSH transport.

Multiple clients can run the same command and register the same frontend port:

```bash
# Backend host A
ssh -N -R 8005:127.0.0.1:22 reverseuser@SERVER -p SSH_BACKEND_PORT

# Backend host B
ssh -N -R 8005:127.0.0.1:22 reverseuser@SERVER -p SSH_BACKEND_PORT
```

The server creates one listener on port `8005` and randomly selects A or B for each accepted connection. Duplicate registrations must therefore be handled by goreverselb's registry, not by attempting a second `net.Listen` for the same port.

Load balancing is per SSH connection, equivalent to current random selection among yamux sessions. A single SSH connection may carry many concurrent channels.

## Instances

The first version has only the empty/default instance. It does not inspect SNI, the custom `PROXY->` preamble, or HTTP CONNECT to choose among SSH backends. All traffic on a frontend port is balanced across every SSH registration for that port.

A future extension may encode service and instance metadata in a structured SSH username, SSH certificate extension, or explicit protocol request. Such an extension must define escaping rather than reusing the current unrestricted colon splitting.

## Connection lifecycle

- One authenticated SSH connection may own one effective frontend port in the first version. Its first successful forwarding request fixes that port.
- Repeating an identical `tcpip-forward` request on the same connection must not create duplicate backend weight. It may return success for the existing registration.
- `cancel-tcpip-forward` removes that connection's registration.
- SSH connection loss removes all registrations owned by the connection.
- Closing the server closes the SSH backend listener, active SSH connections, frontend listeners, and forwarding channels.
- Cleanup must tolerate a cancellation racing with connection loss without double-closing channels, listeners, or the shared shutdown signal.
- Keepalive support should allow standard options such as `ServerAliveInterval` and `ServerAliveCountMax` to detect broken links.

Recommended client invocation:

```bash
ssh -NT \
  -o ExitOnForwardFailure=yes \
  -o ServerAliveInterval=30 \
  -o ServerAliveCountMax=3 \
  -R 8005:127.0.0.1:22 \
  reverseuser@SERVER -p SSH_BACKEND_PORT
```

`ExitOnForwardFailure=yes` is important: authentication success does not imply that the requested frontend port was accepted.

## Error behavior

Failures visible during SSH authentication:

- username mismatch, followed by immediate connection termination
- invalid token
- unsupported authentication method

Failures visible as a rejected `tcpip-forward` request:

- port outside the configured frontend pool
- port is owned by a yamux service or another incompatible frontend
- frontend listener cannot be created
- server is shutting down

After a successful registration, asynchronous backend/channel failures close only the affected frontend connection unless the SSH transport itself is dead. Transport death triggers registration cleanup.

Do not reveal whether a token exists, include configured tokens in errors, or send internal service names to clients.

## Observability

Log structured events for:

- SSH backend listener start/stop
- authentication success/failure without credentials
- forward registration/cancellation
- allocated frontend port
- backend connection loss
- frontend channel-open failure
- last-backend listener cleanup

Include remote address, frontend port, and a generated connection identifier. Do not log passwords, tokens, full SSH authentication payloads, or user configuration.

The REST service listing should eventually expose SSH reverse frontends using their synthetic service name and port, without exposing credentials or backend addresses. This may be implemented with the shared frontend registry; it is not required for initial protocol correctness.

## Security requirements

- Use the existing persistent Ed25519 host key and retain file mode `0600` on creation.
- Require password authentication before processing forwarding requests.
- Compare token values in constant time.
- Apply handshake and authentication timeouts.
- Bound unauthenticated and per-connection resource use.
- Reject all non-forwarding SSH capabilities.
- Validate every `uint32` SSH port before converting it to `int`.
- Use the existing port pool as the authorization boundary; do not permit arbitrary ports outside it.
- Do not trust client-supplied bind addresses to alter listener scope.
- Do not log credentials.

Token-as-password is intentionally compatible with the current shared-secret model, but it exposes the token to SSH client automation and password handling. Public-key or SSH-certificate authentication should be considered separately rather than silently added to this version.

## Implementation outline

1. Add `SSHBackendPort` and `SSHBackendUser` to CLI configuration and the `server` command. Disable the listener when its port is zero and default the username to `reverseuser`.
2. Extract SSH host-key ownership so both consumer-facing `--wrapSSH` and the backend SSH listener reuse the same signer.
3. Add a dedicated backend SSH server with strict password authentication and no shell/session support.
4. Implement `tcpip-forward` and `cancel-tcpip-forward` global-request handlers.
5. Generalize frontend/backend runtime storage around a transport-neutral backend interface.
6. Add SSH registration, duplicate registration, load-balancing, channel-open retry, and cleanup behavior.
7. Wire server shutdown to the SSH listener and active connections.
8. Add REST visibility if the generalized frontend registry makes it available without exposing sensitive data.
9. Update `README.md` with the new server options, environment variables, client commands, public bind behavior, and security limitations.

## Acceptance criteria

### Authentication

- The configured backend username, defaulting to `reverseuser`, plus the configured token authenticates.
- A username mismatch terminates the SSH connection immediately, before password retry or forwarding requests.
- Wrong tokens fail authentication.
- Logs and returned errors never contain the token.

### Registration

- `ssh -N -R 8005:127.0.0.1:22 reverseuser@SERVER` exposes frontend port `8005` when it belongs to the configured pool.
- Dynamic port `0` with the configured backend username allocates and reports a pool port.
- Out-of-range and incompatible occupied ports are rejected.

### Proxying and load balancing

- An incoming frontend TCP connection causes a standard `forwarded-tcpip` channel to reach the local destination configured by `ssh -R`.
- Concurrent frontend connections use independent SSH channels.
- Two SSH clients registering the same port share one public listener and both receive connections over repeated trials.
- A failed channel open can select another registered SSH backend before any payload is delivered.
- Half-closing one side does not truncate the opposite response direction.

### Lifecycle

- `cancel-tcpip-forward` removes only the requesting connection's registration.
- Disconnecting one of multiple backends leaves the frontend active.
- Disconnecting the last backend closes the frontend and returns its port to the pool.
- Reconnect can reclaim the released port.
- Server shutdown terminates the backend SSH listener, active SSH connections, channels, and frontends without panic or deadlock.

## Test plan

Add focused Go tests using loopback listeners and `golang.org/x/crypto/ssh` clients:

- Username validation defaults to `reverseuser`, honors configuration overrides, and immediately terminates mismatched connections before password retries or request handling.
- Constant-time authentication result behavior at the API level without timing assertions.
- `tcpip-forward` payload parsing and one-port-per-connection constraints.
- Dynamic allocation, explicit allocation, collision, cancellation, and pool return.
- Duplicate registration on one connection does not add load-balancing weight.
- Multiple SSH connections on one port receive `forwarded-tcpip` channels.
- Channel-open failure retries another backend only before payload forwarding.
- Connection-loss cleanup races safely with explicit cancellation.
- End-to-end forwarding to a loopback echo server, including concurrent streams and TCP half-close behavior.
- Coexistence test proving the TLS/yamux listener and SSH backend listener can operate simultaneously on different control ports.

After implementation, run:

```bash
gofmt -w <changed-go-files>
go test -vet=off ./...
go build ./cmd/goreverselb
```

Also run an OpenSSH interoperability test with `ExitOnForwardFailure=yes`; an in-process Go SSH client alone does not prove compatibility with the intended binary-free workflow.
