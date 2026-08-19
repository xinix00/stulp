# Stulp

*Your apps. Your devices. Your home.*

A small controller for the home, written in Go. Install an app, pair a device,
control it, configure it, and connect it to other devices in a Flow. That loop
is the product; everything here exists to make it calmer or more reliable.

Stulp keeps no database engine. All platform state lives in one portable JSON
document, because the data is small, the questions are simple, and there is
deliberately no history to query. A sensor reporting a temperature costs no disk
at all.

## Apps are Go plugins

An app is an ordinary program: a `main()`, a `go build`, one binary. Stulp starts
it, hands it one end of a socketpair, and the two speak a small length-prefixed
protocol over it.

```go
func main() {
	appsdk.Run(appsdk.Plugin{
		Drivers: map[string]appsdk.Driver{"lamp": lampDriver{}},
	})
}
```

One process per app, so a plugin that hangs or crashes takes nothing else down
and there is no way for one app to reach another — the socketpair has no
address, so there is nothing to address. Each binary carries only what it
references. Reads come from a copy Stulp pushes at the handshake and keeps
current, so asking a device its name is a map lookup and not a round trip.

The SDK and included apps are deliberately ordinary Go, so the implementation
itself remains the most precise description of the plugin contract.

## Matter, without a radio

Matter is multi-admin, so a device already commissioned in Apple Home can be
shared with Stulp over the network. The Apple TV keeps acting as the Thread
border router and Stulp joins as an equal second controller — no Matter or
Thread radio of its own.

That covers TLV, onboarding codes, DNS-SD discovery, SPAKE2+ and CASE, real
attestation checks, subscriptions, and a mesh view built from what the nodes
themselves report.

Native BLE, Zigbee, Z-Wave and RF stacks are out of scope on purpose: Stulp is
an on-network controller and relies on the ecosystem that is already in the
house.

## Layout

| | |
|---|---|
| `internal/appsdk` | what a plugin is written against — no JavaScript anywhere |
| `internal/appsdk/udp` | multicast, broadcast and the socket options Go's `net` omits |
| `internal/plugin` | Stulp's side: start the binary, speak the protocol |
| `internal/appproto` | the wire protocol, with a frame cap checked before allocation |
| `internal/store` | the one JSON document, and everything that watches it |
| `internal/flow` | IF/AND/THEN cards and the engine that runs them |
| `internal/webapi` | the keyed Manage interface, its private browser API and MCP |
| `internal/supervisor` | starting, restarting and reporting on apps |
| `plugins/matter` | the Matter app — its own process, `internal/` and all |

## Running it

```sh
./build.sh
./stulp --document stulp.json serve --token EEN-LANGE-WILLEKEURIGE-SLEUTEL
```

Open Manage at `http://127.0.0.1:8080/EEN-LANGE-WILLEKEURIGE-SLEUTEL`.
The same key exposes the stateless MCP server at
`http://127.0.0.1:8080/mcp/EEN-LANGE-WILLEKEURIGE-SLEUTEL`. Visiting the Manage
URL establishes an HttpOnly browser session; there is no API-key field in the
interface and the private browser API does not accept Bearer tokens. Without
`--token`, local development remains open but MCP stays disabled.

`build.sh` builds the controller and every app in `plugins/*`, each one to its
own directory under its app id — which is where Stulp looks for it. Name one or
more targets to build just those (`./build.sh matter nibe`), and `GOOS`/`GOARCH`
work as usual for cross-compiling. It is a plain `go build` underneath:

```sh
go build -ldflags="-s -w" -o stulp ./cmd/stulp
```

`-s -w` drops the DWARF tables and the symbol table: 3.2 MB of the 10.5, doing
nothing at runtime. Panic stack traces still name their functions and lines,
because those come from the pclntab rather than from DWARF. Leave the flags off
when you want a debugger to attach.

Stulp starts every app itself. An app that cannot be started that way — one that
ships as a container, or one you want to hold in a debugger — can start first and
announce itself instead:

```sh
./stulp --document stulp.json serve --attach /tmp/stulp-attach.sock
STULP_ATTACH=/tmp/stulp-attach.sock ./plugins/lamp/com.example.lamp
```

Nothing past the handshake changes; the app is a normal app from there. For an app
in its own pod there is `--attach-port`, which needs a per-app token
(`stulp attach-token APP_ID`) because a port has no uid to ask about. The token is
never sent: Stulp opens with a nonce and the app answers with an HMAC of it, both
ways, so a listener has nothing to reuse. TLS on top is what makes the traffic
private, and `--attach-port` insists on it unless you say `--attach-plaintext`.
Without either flag there is nothing listening and no way in.

## License

Stulp is available under the [MIT License](LICENSE).
