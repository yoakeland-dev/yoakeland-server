# Yoakeland Backend IPC Specification (v1.0)

This document is the complete, authoritative specification for the `yoakeland` backend daemon. It provides all information necessary to build a graphical frontend (e.g., Flutter, QML, Electron) that communicates with `yoakeland` over standard input/output.

---

## 1. Architectural Overview & Process Model

```text
┌──────────────────────────────────────────────────────────────────┐
│                   Frontend Process (GUI)                         │
│                                                                  │
│  [State Store] ◄─── JSON-lines Parser ◄─── Child Process stdout  │
│  [User Action] ───► JSON-lines Writer ───► Child Process stdin   │
└─────────────────────────────────┬────────────────────────────────┘
                                  │ stdio (newline-delimited JSON)
┌─────────────────────────────────▼────────────────────────────────┐
│                  yoakeland Daemon (Backend)                      │
│                                                                  │
│  • Normalizer (Argv & Path Sandbox)                              •
│  • Myers Diff Engine                                             •
│  • Permission Engine (.yoake/permissions.json)                   •
│  • Snapshot & Rollback Store (.yoake/snapshots/)                 •
│  • Multi-Turn Session Store (.yoake/sessions/)                   •
│  • Isolated Subprocess Boundary (Bubblewrap / Linux Namespaces)  •
└──────────────────────────────────────────────────────────────────┘
```

### Process Lifecycle
1. **Spawn**: The frontend launches `yoakeland` as a standalone child process.
2. **Communication**: All IPC happens strictly via `stdin` (inbound to backend) and `stdout` (outbound from backend).
3. **Stderr**: The backend reserves `stderr` for operational logs, panic traces, and low-level sandbox launch logs. The frontend should stream `stderr` to a debugging console or log file.
4. **Shutdown**: 
   - Clean shutdown: Close the child process's `stdin` pipe (backend receives `EOF` and cleanly shuts down).
   - Hard termination: Send `SIGTERM` (or `SIGKILL` on timeout).

### Invocation & Command-Line Flags

```bash
yoakeland [flags]
```

| Flag | Type | Default | Description |
|---|---|---|---|
| `-project` | `string` | `.` | Absolute or relative path to the workspace root directory. |
| `-provider` | `string` | `auto` | LLM Provider: `anthropic`, `openai`, `openrouter`, or `auto` (auto-detects from environment variables). |
| `-model` | `string` | *(provider default)* | Model ID (e.g. `claude-3-5-sonnet-20241022`, `gpt-4o`, `deepseek/deepseek-chat`). |
| `-api-key` | `string` | `""` | Optional API key override. If omitted, uses environment variables (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, etc.). |
| `-base-url` | `string` | *(provider default)* | Custom API endpoint. Enables local endpoints like LM Studio (`http://localhost:1234/v1`) or Ollama (`http://localhost:11434/v1`). |

---

## 2. Framing & Envelope Protocol

### Framing Rules
- **Encoding**: UTF-8.
- **Delimiter**: Strictly Newline-Delimited JSON (`\n` or `0x0A`). Carriage returns (`\r`) must not precede the newline.
- **Maximum Frame Size**: 10 MB per line (allows for large file reads and tool outputs).
- **Concurrency**: Inbound frames can be sent at any time (e.g., an `EventCancelTurn` can be sent while the backend is streaming `EventModelDelta`).

### The Universal Envelope Schema

Every frame sent in either direction conforms to the following JSON structure:

```typescript
interface Envelope {
  type: EventType;            // Identifies the payload shape
  sessionId?: string;         // Identifies the conversation session
  turnId?: string;            // Correlates a specific turn lifecycle
  toolCallId?: string;        // Correlates a specific tool invocation
  data?: Record<string, any>; // Event-specific payload object
}
```

---

## 3. Event Dictionary (Wire Contract)

### 3.1 Inbound Events (Frontend → Backend)

#### `user_prompt`
Initiates a new conversational turn or prompt submission.
- **Correlation**: Frontend provides a unique `turnId` (e.g. `turn-1`) and `sessionId`.
```json
{
  "type": "user_prompt",
  "sessionId": "session-main",
  "turnId": "turn-1",
  "data": {
    "prompt": "Refactor the file reader to support streaming",
    "sessionId": "session-main"
  }
}
```

#### `permission_response`
Answers an outstanding `permission_request` from the backend.
- **Correlation**: `toolCallId` **must** match the `toolCallId` received in `permission_request`.
```json
{
  "type": "permission_response",
  "sessionId": "session-main",
  "turnId": "turn-1",
  "toolCallId": "call_abc123",
  "data": {
    "allowed": true,
    "remember": true
  }
}
```
- `allowed`: If `true`, the backend executes the tool. If `false`, execution is denied and a denial message is fed to the model.
- `remember`: If `true`, saves the decision in `<project>/.yoake/permissions.json` so the user is not prompted again for this command/path pattern.

#### `cancel_turn`
Immediately halts an ongoing turn.
- **Correlation**: Targets the active `turnId`.
```json
{
  "type": "cancel_turn",
  "sessionId": "session-main",
  "turnId": "turn-1",
  "data": {
    "sessionId": "session-main",
    "turnId": "turn-1"
  }
}
```
*Effect*: Aborts the model's streaming HTTP connection, terminates any child processes executing inside the sandbox via `SIGKILL`, cancels outstanding permission waits, and emits `turn_completed` with `finishReason: "cancelled"`.

#### `restore_snapshot`
Reverts the project filesystem to a previous snapshot state.
```json
{
  "type": "restore_snapshot",
  "sessionId": "session-main",
  "turnId": "",
  "data": {
    "snapshotId": "snap_1710950400000000"
  }
}
```

---

### 3.2 Outbound Events (Backend → Frontend)

#### `model_delta`
A real-time token or text chunk streamed by the model.
```json
{
  "type": "model_delta",
  "sessionId": "session-main",
  "turnId": "turn-1",
  "data": {
    "content": "I will inspect the "
  }
}
```
*UI Action*: Append `data.content` directly to the active assistant message widget.

#### `tool_call_requested`
Notifies the frontend that the model has formally requested a tool call.
```json
{
  "type": "tool_call_requested",
  "sessionId": "session-main",
  "turnId": "turn-1",
  "toolCallId": "call_abc123",
  "data": {
    "name": "file_write",
    "args": {
      "path": "lib/main.dart",
      "content": "void main() => runApp(App());"
    }
  }
}
```
*UI Action*: Render an in-progress tool execution card/indicator.

#### `permission_request`
The backend pauses execution and requires user approval before executing a tool.
```json
{
  "type": "permission_request",
  "sessionId": "session-main",
  "turnId": "turn-1",
  "toolCallId": "call_abc123",
  "data": {
    "toolName": "file_write",
    "description": "file_write on lib/main.dart",
    "requiresAsk": true,
    "cacheKey": "file_write:lib/main.dart",
    "diff": {
      "filePath": "lib/main.dart",
      "unified": "--- a/lib/main.dart\n+++ b/lib/main.dart\n@@ -1,3 +1,1 @@\n-void old() {}\n+void main() => runApp(App());\n",
      "hunks": [
        {
          "oldStart": 1,
          "oldLines": 3,
          "newStart": 1,
          "newLines": 1,
          "lines": [
            { "type": "-", "content": "void old() {}" },
            { "type": "+", "content": "void main() => runApp(App());" }
          ]
        }
      ]
    }
  }
}
```
*UI Action*: Display an interactive modal or inline prompt. If `data.diff` is non-null, render a side-by-side or unified diff inspector.

#### `tool_result`
The result of executing a tool (after approval or if pre-approved by policy).
```json
{
  "type": "tool_result",
  "sessionId": "session-main",
  "turnId": "turn-1",
  "toolCallId": "call_abc123",
  "data": {
    "toolName": "file_write",
    "output": "Successfully wrote 32 bytes to lib/main.dart",
    "error": ""
  }
}
```
*Note*: If the user denied permission, `data.error` will contain `"permission denied by user or policy"`.

#### `turn_completed`
Signifies that the iterative turn loop has ended.
```json
{
  "type": "turn_completed",
  "sessionId": "session-main",
  "turnId": "turn-1",
  "data": {
    "finishReason": "stop",
    "sessionId": "session-main",
    "usage": {
      "inputTokens": 1420,
      "outputTokens": 380,
      "totalTokens": 1800
    },
    "durationMs": 2340
  }
}
```
- `finishReason`: `"stop"` (normal completion) or `"cancelled"` (user aborted).
- `usage`: Token consumption breakdown.
- `durationMs`: Wall-clock execution time for the entire turn.

#### `snapshot_restored`
Confirms successful restoration of a filesystem snapshot.
```json
{
  "type": "snapshot_restored",
  "sessionId": "session-main",
  "data": {
    "snapshotId": "snap_1710950400000000"
  }
}
```
*UI Action*: Refresh any open editor buffers or tree views; notify user of restoration.

#### `error`
Reports operational or fatal backend errors.
```json
{
  "type": "error",
  "sessionId": "session-main",
  "turnId": "turn-1",
  "data": {
    "message": "anthropic error (401): invalid api key",
    "fatal": false
  }
}
```

---

## 4. State Machine & Turn Lifecycle

```text
 ┌───────────┐
 │   IDLE    │◄────────────────────────────────────────────────┐
 └─────┬─────┘                                                 │
       │ user_prompt                                           │
       ▼                                                       │
 ┌───────────┐                                                 │
 │ STREAMING ├────────► model_delta                            │
 └─────┬─────┘                                                 │
       │                                                       │
       ├─────────────────┐                                     │
       │ no tool calls   │ tool_call_requested                 │
       │                 ▼                                     │
       │          ┌─────────────┐                              │
       │          │ PERM EVAL   │                              │
       │          └──────┬──────┘                              │
       │                 │                                     │
       │         ┌───────┴────────┐                            │
       │         │ allowed        │ requires ask               │
       │         │                ▼                            │
       │         │         ┌─────────────┐                     │
       │         │         │ WAIT PERM   │                     │
       │         │         └──────┬──────┘                     │
       │         │                │ permission_response        │
       │         ▼                ▼                            │
       │   ┌─────────────────────────────┐                     │
       │   │       TOOL EXECUTION        ├─► tool_result       │
       │   └─────────────┬───────────────┘                     │
       │                 │ (loop back for next model response) │
       │                 └─────────────────────────────────────┤
       ▼                                                       │
 ┌───────────┐                                                 │
 │ COMPLETED │ turn_completed                                  │
 └─────┬─────┘                                                 │
       └───────────────────────────────────────────────────────┘
```

### Cancellation Semantics
At **any** point while not in `IDLE` (`STREAMING`, `WAIT PERM`, or `TOOL EXECUTION`), the frontend may send:
```json
{"type": "cancel_turn", "turnId": "<currentTurnId>"}
```
The backend guarantees:
1. Active model streaming context is canceled.
2. Any blocking `WAIT PERM` is unblocked.
3. Any running subprocess in the sandbox is terminated.
4. The turn terminates with a single `turn_completed` frame containing `finishReason: "cancelled"`.
5. The state returns to `IDLE`.

---

## 5. Structured Diff Rendering (Myers Diff Engine)

When `permission_request` is received for a `file_write` or `file_edit` tool, the `data.diff` field provides pre-computed diff information.

### Diff Data Types
```typescript
interface FileDiff {
  filePath: string;     // Relative path to file
  unified: string;      // Standard unified diff format
  hunks: Hunk[];        // Structured hunks for GUI rendering
}

interface Hunk {
  oldStart: number;
  oldLines: number;
  newStart: number;
  newLines: number;
  lines: DiffLine[];
}

interface DiffLine {
  type: " " | "+" | "-"; // " " = Context, "+" = Added, "-" = Deleted
  content: string;       // Text content (without prefix symbol)
}
```

### Rendering Guidelines for the Frontend
- If `type == "+"`, render green background tint.
- If `type == "-"`, render red background tint.
- If `type == " "`, render neutral editor background.
- Display `filePath` in the modal title bar.
- Provide three user actions:
  1. **Allow Once**: `{"allowed": true, "remember": false}`
  2. **Always Allow**: `{"allowed": true, "remember": true}`
  3. **Deny**: `{"allowed": false, "remember": false}`

---

## 6. Project Artifacts & Filesystem Layout

The backend maintains persistent runtime state in a hidden `.yoake/` directory inside the project root:

```text
<project_root>/
└── .yoake/
    ├── permissions.json        # Persisted permission rules
    ├── sessions/
    │   └── <sessionId>.json    # Full conversation history & tool invocations
    └── snapshots/
        └── snap_<timestamp>/   # File-level backups taken before mutations
            ├── manifest.json   # Files touched & rollback instructions
            └── data/           # Original pre-mutation byte copies
```

### Restoring Snapshots
The frontend can prompt the user to revert a turn by sending:
```json
{
  "type": "restore_snapshot",
  "data": { "snapshotId": "snap_1710950400000000" }
}
```
The backend restores modified files to their exact pre-turn state and removes any files created during that turn.

---

## 7. Reference Frontend Implementation (Dart / Flutter)

Below is a complete, minimal Dart client demonstrating how to wire the subprocess, serialize JSON-lines, and expose a reactive stream.

### `lib/ipc/yoake_backend.dart`

```dart
import 'dart:async';
import 'dart:convert';
import 'dart:io';

class Envelope {
  final String type;
  final String? sessionId;
  final String? turnId;
  final String? toolCallId;
  final Map<String, dynamic>? data;

  Envelope({
    required this.type,
    this.sessionId,
    this.turnId,
    this.toolCallId,
    this.data,
  });

  factory Envelope.fromJson(Map<String, dynamic> json) {
    return Envelope(
      type: json['type'] as String,
      sessionId: json['sessionId'] as String?,
      turnId: json['turnId'] as String?,
      toolCallId: json['toolCallId'] as String?,
      data: json['data'] as Map<String, dynamic>?,
    );
  }

  Map<String, dynamic> toJson() => {
    'type': type,
    if (sessionId != null) 'sessionId': sessionId,
    if (turnId != null) 'turnId': turnId,
    if (toolCallId != null) 'toolCallId': toolCallId,
    if (data != null) 'data': data,
  };
}

class YoakeBackend {
  Process? _process;
  final _eventController = StreamController<Envelope>.broadcast();
  Stream<Envelope> get events => _eventController.stream;

  Future<void> start({
    required String backendBinPath,
    required String projectDir,
    String provider = 'auto',
    String? model,
    String? baseUrl,
  }) async {
    final args = ['-project', projectDir, '-provider', provider];
    if (model != null && model.isNotEmpty) args.addAll(['-model', model]);
    if (baseUrl != null && baseUrl.isNotEmpty) args.addAll(['-base-url', baseUrl]);

    _process = await Process.start(backendBinPath, args);

    // Parse newline-delimited JSON from stdout
    _process!.stdout
        .transform(utf8.decoder)
        .transform(const LineSplitter())
        .listen((line) {
      if (line.trim().isEmpty) return;
      try {
        final map = jsonDecode(line) as Map<String, dynamic>;
        _eventController.add(Envelope.fromJson(map));
      } catch (e) {
        stderr.writeln('Failed to decode IPC frame: $e\nLine: $line');
      }
    });

    // Pipe operational errors from backend stderr
    _process!.stderr
        .transform(utf8.decoder)
        .listen((err) => stderr.write('[Backend Log] $err'));
  }

  void send(Envelope env) {
    if (_process == null) throw StateError('Backend is not running');
    final line = jsonEncode(env.toJson()) + '\n';
    _process!.stdin.write(line);
  }

  void sendPrompt(String sessionId, String turnId, String prompt) {
    send(Envelope(
      type: 'user_prompt',
      sessionId: sessionId,
      turnId: turnId,
      data: {'prompt': prompt, 'sessionId': sessionId},
    ));
  }

  void sendPermissionResponse({
    required String sessionId,
    required String turnId,
    required String toolCallId,
    required bool allowed,
    required bool remember,
  }) {
    send(Envelope(
      type: 'permission_response',
      sessionId: sessionId,
      turnId: turnId,
      toolCallId: toolCallId,
      data: {'allowed': allowed, 'remember': remember},
    ));
  }

  void cancelTurn(String sessionId, String turnId) {
    send(Envelope(
      type: 'cancel_turn',
      sessionId: sessionId,
      turnId: turnId,
      data: {'sessionId': sessionId, 'turnId': turnId},
    ));
  }

  Future<void> dispose() async {
    await _process?.stdin.close();
    _process?.kill();
    await _eventController.close();
  }
}
```

---

## 8. Summary Checklist for Frontend Developers

- [ ] **Single Stdin Writer**: Never open multiple writers to backend `stdin`.
- [ ] **Frame Buffer**: Ensure line splitting buffers up to 10 MB chunks.
- [ ] **Tool Call Correlation**: Always echo back `toolCallId` in `permission_response`.
- [ ] **Active Turn State**: Maintain a boolean `isTurnActive` that enables/disables the UI "Stop" button.
- [ ] **Diff Preview**: Check `event.data['diff']` when receiving `permission_request` to display visual diff hunks.
- [ ] **Session Persistence**: Pass `sessionId` in each prompt to preserve conversational context across application restarts.