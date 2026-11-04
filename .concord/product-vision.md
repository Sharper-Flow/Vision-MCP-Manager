# Vision product vision

This document states what Vision is for and how its design choices are judged.
It is direction, not law: binding requirements live in `.concord/law/`.

## Purpose

Vision is the one local MCP gateway that every agent session on a host connects
to. It owns the lifecycle of MCP servers so that agents and their clients do not
have to: it starts them, shares or isolates them, checks they work, recovers
them, and holds their credentials.

## The problem

An agent client that connects to MCP servers directly owns each connection
itself. On a host that runs many agent sessions at once, that costs:

- **Duplicate processes.** Each session starts its own copy of every local
  server, multiplying memory use and startup time.
- **No recovery.** When a server process dies, the session loses its tools
  until someone reconnects it.
- **No shared view.** Nothing reports which servers are healthy across all
  sessions, or which session holds which capacity.
- **Scattered credentials.** API keys end up in each client's configuration,
  in subprocess arguments, or in logs.
- **No common policy.** Limits, timeouts and access rules are set per client,
  if at all.

## Who it serves

- **The operator**, who runs many concurrent agent sessions on one host and
  needs one place to configure, observe and repair MCP servers.
- **Agent sessions**, which need MCP tools that are available when called,
  return honest errors when they are not, and do not interfere with each other.

## Goals

1. **One endpoint per server for every session.** Clients connect to Vision's
   endpoints, for local and remote MCP servers alike. A client's own
   configuration names Vision, not each vendor.
2. **Share where safe, isolate where needed.** One server process serves many
   sessions by default. A server whose state must not leak between sessions gets
   its own process or session per client.
3. **Recover without the client.** A failed server is detected and replaced
   without a client restart. Lost in-memory state is reported, not hidden.
4. **Report what is true.** Status reflects what a probe proved, not that a
   process exists. A server with no working endpoint is never reported healthy.
5. **Keep credentials out of sight.** Secrets are resolved only when a
   connection needs them. They never appear in arguments, logs, error text,
   status output, or a file Vision writes.
6. **Bound shared capacity.** Session limits, concurrency limits and circuit
   breakers protect a shared server from any one session.
7. **Shape what agents see.** Vision can narrow and clarify the tools a server
   offers, so agents load fewer tools and get better-typed results, without a
   change to the server.
8. **Forward MCP faithfully.** Anything a server offers a direct client, a client
   through Vision also receives, unless the operator chose to hide it.

## Principles

- **At most once.** Vision forwards each application request at most once. A
  request whose outcome is uncertain is never replayed.
- **Clients stay unchanged.** Vision fixes server-side problems inside Vision.
  It does not require changes to the agent client's core.
- **Limits expose faults, they do not contain them.** When sessions pile up
  against a cap, the leak gets fixed. Raising the cap is not the fix.
- **Configuration means what it says.** Every setting Vision accepts takes
  effect. A setting with no runtime effect is removed, not documented around.
- **Local by default.** Endpoints bind to loopback. Vision manages one host's
  servers; it is not a multi-host service.
- **Claims follow code.** Documentation describes the behavior the code has
  today, not a planned one.

## Non-goals

- Replacing the agent client's MCP support. Vision is a gateway between the
  client and servers, not a second client.
- Retrying tool calls on the agent's behalf.
- Letting agents install new servers. Every server comes from operator
  configuration.
- Serving clients on other hosts.

## Open questions

- Whether the OpenCode plugin should keep its own wrappers for the admin tools,
  or leave admin access entirely to Vision's own MCP endpoint.
- Which server classes gain from Vision and which only pay the extra hop. No
  measurement of latency, memory or process savings exists yet.

## Signs it is working

- Starting another agent session starts no new process for a shared server.
- A killed server process is replaced without restarting any client session.
- No status surface calls a server healthy while its endpoint refuses requests.
- No secret value appears in any file, log or response Vision produces.
- A client through Vision sees the same tools, resources and prompts as a
  direct connection, minus what the operator chose to hide.
