# Yoakeland Client Build Checklist (Tauri)

## Context

`yoakeland` (this repo) is a Go backend that speaks newline-delimited JSON over stdio or a TCP socket — see `SPEC.md` for the complete, authoritative wire protocol (transport modes, auth handshake, every event, the provider registry, spend/context reporting). This checklist is for building the first real GUI client against it, chosen to be **Tauri**: a Rust host process plus a native webview, so the app can run wherever the user is while the project files and the model calls stay on whichever machine runs `yoakeland` itself (possibly the same machine, possibly not — server mode is what makes that split possible).

If you're picking this up in a fresh session: read `SPEC.md` first, in full. It is the source of truth for every event name, payload shape, and behavior referenced below. This checklist won't repeat the wire details — it tells you what to build and in what order.

## The one architectural rule that matters

**Separate the logic from the interface, strictly enough that the interface can be thrown away and rebuilt without touching the logic.** Concretely:

- All protocol handling, connection management, reconnection, session state, and credential handling lives in **Rust**, in a module/crate that has zero knowledge of how anything is rendered. Call it the **core**.
- The webview (whatever JS framework you pick — plain, React, Svelte, whatever) is the **interface**: it renders state and forwards user actions. It never parses a wire envelope, never opens a socket, never retries a connection. It talks to the core exclusively through Tauri's command/event bridge (`invoke` calls in, `emit`/`listen` events out), using plain, already-decoded data structures the interface can trust to be correct.
- Concretely, the boundary is: **core owns everything up to and including "here is the current state of session X as a plain struct"; interface owns everything from "here is how to draw that struct" onward.** If you find UI code deciding what a `session_taken_over` event *means*, that logic slipped past the boundary and belongs in core instead.
- Why this matters here specifically: the interface is genuinely likely to be redesigned or partially replaced (different framework, a second surface like a CLI or a menu-bar widget) while the protocol/session logic won't need to change nearly as often. Keeping them decoupled means that redesign is a webview-only change.
- Concretely test the boundary before moving on from Phase 1: you should be able to write a unit test for reconnect logic, provider switching, or session-takeover handling *with no webview running at all* — if you can't, the core has a UI dependency it shouldn't.

## Suggested project layout

Actual layout: the client lives in the sibling `yoake-gui/` repo, not nested under `yoakeland/` — everything below `client/` in this diagram is `yoake-gui/`'s repo root instead.

```
yoakeland/
├── SPEC.md
├── CLIENT_CHECKLIST.md          (this file)
├── cmd/, internal/...           (existing Go backend)
└── client/                      (new)
    ├── src-tauri/
    │   ├── src/
    │   │   ├── core/            # connection, protocol types, session state, reconnect — no UI knowledge
    │   │   │   ├── protocol.rs  # Envelope + every Data struct from SPEC.md §3, serde-derived
    │   │   │   ├── connection.rs# TCP/stdio transport, auth handshake, framing, reconnect/backoff
    │   │   │   ├── session.rs   # per-session state machine mirroring SPEC.md §4/§4a
    │   │   │   └── providers.rs # provider registry client-side mirror, OAuth flow driver
    │   │   ├── commands.rs      # #[tauri::command] functions — the ONLY thing the webview calls
    │   │   └── main.rs
    │   └── Cargo.toml
    └── src/                      # webview frontend — interface only
        └── ...
```

Adjust names/framework to taste; the point is the `core/` vs everything-else split, not the exact file names.

---

## Phase 0 — Scaffolding
- [x] `cargo install tauri-cli` (or use `npm create tauri-app`) and scaffold `client/` with your chosen webview framework
- [x] Confirm a scaffolded Tauri app builds and runs a "hello world" window before writing any yoakeland-specific code
- [x] Add `serde`/`serde_json` for the protocol types, `tokio` for async networking

## Phase 1 — Core: protocol & connection (no UI yet)
- [x] Port every `Envelope`/`...Data` struct from `SPEC.md` §3 into Rust with `serde(rename_all = "camelCase")` — this is a direct transcription, not a redesign; keep field names identical to the wire so you're not maintaining a translation layer
- [x] Implement the transport: a TCP connection (`tokio::net::TcpStream`) that frames on `\n`, matching `SPEC.md` §2's rules (10MB max line, no bare `\r`)
- [x] Implement the `auth` handshake exactly as `SPEC.md` §2a describes: send `auth` first, wait for either normal traffic to start flowing or a fatal `error`, surface connection-refused distinctly from auth-refused
- [x] Implement reconnect/backoff for a dropped connection — decide and document your policy (e.g. exponential backoff, cap, give up after N attempts and surface a "disconnected" state to the interface layer)
- [x] Write unit tests for framing and auth against a fake in-process TCP server (no real `yoakeland` binary needed) — verify at least: successful auth, wrong token, malformed first frame, connection drop mid-turn
- [x] Verify all of this against a **real** running `yoakeland -listen :7890 -token ...` process before moving on — a fake-server test suite that never touched the real binary can still be wrong about something the docs got subtly wrong

## Phase 2 — Core: session state
- [x] Model one session as its own state machine per `SPEC.md` §4 (`Idle`/`Streaming`/`WaitingPermission`/`ToolExecution`/`Completed`), and a registry of many concurrent sessions per §4a — mirror the backend's own concurrency model, don't collapse it back into "one conversation" in the client
- [x] Handle every outbound event type from `SPEC.md` §3.2 by updating the relevant session's state: `model_delta` (append), `tool_call_requested`, `permission_request` (including the `diff` field — §5), `tool_result`, `turn_completed` (including the new `context`/`spend` fields), `snapshot_restored`, `error`, `session_taken_over`
- [x] Implement `session_taken_over` handling as a real state transition (e.g. a `TakenOver` session state), not just a toast — a displaced session must stop expecting further events
- [x] Expose session state to the interface via Tauri commands (`get_session_state`, `list_sessions`) and events (`emit` on every state change) — the interface should be able to reconstruct its entire view from these, never by inspecting raw envelopes itself
- [x] Unit test: two sessions running concurrently don't leak state into each other (mirror the backend's own `TestRegistry_SessionsAreIsolated` guarantee on the client side)

## Phase 3 — Core: providers
- [x] Implement `register_provider`/`list_providers`/`set_provider` request/response as core commands (`SPEC.md` §3.1/§3.2, §6b) — plus `remove_provider`/`detect_cli`/`request_session_list`, added once SPEC.md picked them up
- [~] Implement the OpenRouter OAuth flow (§6b): `openrouter_oauth_start` → open the returned URL in the system browser → capture the callback → `openrouter_oauth_complete`. Wire-level start/complete commands work end to end, and the URL opens via `tauri-plugin-opener` (already a dependency — no new one added). **Simplification**: no loopback HTTP listener or custom URL scheme to auto-capture the callback; the UI has the user paste the `code` query param back in by hand. Upgrade path: add a `tiny_http`/`hyper` listener on the `callbackUrl` port in `commands.rs` when this needs to be one-click.
- [x] Detect CLI-subscription availability (`cli_claude`/`cli_codex`) — SPEC.md picked up a real `detect_cli`/`cli_availability` wire event since this checklist was written, so this is now a normal request/response, no workaround needed
- [x] Unit test: registering, listing (credential scrubbed), and switching providers against a fake server harness — `core/providers.rs`'s `fake_server_roundtrip` test drives this through the real `connection::spawn` transport, plus a dedicated test proving `ProviderSummary` structurally can't carry an `apiKey` even if a buggy server sent one

## Phase 4 — Interface: core chat experience
- [x] Session list / switcher (multiple concurrent sessions, per Phase 2) — a `<select>` populated from every session seen so far, switchable
- [x] Message thread view consuming `model_delta`/`tool_result` state, not raw events
- [x] Permission prompt UI rendering the diff (unified text, not a hunk-by-hunk/colored view — see PoC note below) per `SPEC.md` §5 (Allow Once / Always Allow / Deny)
- [x] Cancel/stop button wired to `cancel_turn`, gated on that session's own `isTurnActive`, not a global flag
- [x] Snapshot list + restore action wired to `restore_snapshot`/`snapshot_restored` — restore only; there's no wire event to enumerate existing snapshots yet, so "list" is just an id text field

## Phase 5 — Interface: provider & ops UI
- [x] Provider picker sourced from `list_providers` — no hardcoded provider list
- [x] Manual API key registration form
- [x] OpenRouter "sign in" button driving the OAuth flow from Phase 3 (manual code paste, see above)
- [x] Context meter and running-spend display from `turn_completed.data.context`/`.spend` — plain text readout, not a graphical meter
- [x] Connection/auth screen: server address + token entry, connection status (all `ConnectionState` variants incl. reconnect attempt/backoff), reconnect state

## Phase 6 — Polish
- [~] Persist connection config — only the server address is persisted (`localStorage`, not a plaintext file on disk either). **Simplification**: the token is intentionally never persisted (typed fresh each run) since this Tauri setup has no keychain plugin wired up yet; adding `tauri-plugin-stronghold` or the OS-keychain plugin is the real fix before this token entry is anything but a dev convenience.
- [x] Handle the backend process not being reachable at all (vs. reachable-but-auth-failed vs. reachable-and-fine) as three distinct, distinguishable UI states — `ConnectionState` already carries all of `connecting`/`connected`/`authFailed`/`disconnected`/`reconnecting`/`gaveUp`, rendered as text
- [ ] Decide and implement a policy for "session already open on another device" from the *opening* side too (§2a explicitly leaves this to the frontend) — not built; `request_session_list`/`session_list` data is shown but nothing warns before an open that would take over a session

## Also picked up along the way (SPEC.md additions since this checklist was first written)
- [x] `ping`/`pong` liveness (§2a): the core replies to an unsolicited `ping` automatically (transport-level, never surfaced to the interface) and treats ~75s of silence from the backend as dead, forcing a reconnect
- [ ] TLS (§2a): not implemented — the client only ever dials plaintext TCP. Fine for the localhost dev loop this was tested against; a real deployment crossing a network needs this before the "TLS Awareness" line in §8 is actually true of this client.
- [x] `finishReason: "budget_exceeded"` (§3.2): no special-cased handling needed — `TurnCompletedData.finish_reason` was always a plain string, so this "just works" and shows up verbatim in the turn-meta readout; a nicer PoC would style it differently from `"stop"`

## Backend change since this checklist (and the client built from it) were written: multi-project support

**This is no longer a non-goal — the backend now does this for real, and the client above does not yet.** As of SPEC.md v2.1 (§4b), a single server process serves many projects at once, opened on demand, each fully isolated (own sandbox/permissions/snapshots/sessions). Concretely, this changes wire behavior the existing client's core assumed was fixed:
- `user_prompt` now carries an optional `projectPath` (bound to a `sessionId` on first use, permanently — a later conflicting path is rejected, not switched)
- `list_sessions` now requires a `projectPath` (there's no more "the one project," so "list everything" isn't well-defined without it)
- Two new events exist: `open_project`/`project_opened` and `list_projects`/`project_list`

The already-built client will keep working for the single-default-project case (the server still supports `-project` as a fallback default, so a client that never sends `projectPath` behaves exactly as before) — but it cannot yet do "pick a folder on the fly, work on several at once," which was the actual point of building this. Picking that up means: extend `core/protocol.rs`'s types for the new fields/events, add `projectPath` plumbing to session creation, add `open_project`/`list_projects` core commands, and add a project switcher to the interface alongside the existing session switcher (Phase 4). Treat it as a new phase on the existing checklist, not a rewrite — everything through Phase 6 above is still valid, this only adds a project dimension on top of the session one.

## Explicit non-goals for this pass
Don't build these yet — they're real gaps but not required for a working v1 client, and pulling them in now will blur the logic/interface boundary before it's proven out:
- Mobile packaging
