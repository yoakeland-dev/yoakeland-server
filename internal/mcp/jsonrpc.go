// Package mcp is yoakeland's Model Context Protocol client: it connects to the
// MCP servers a user configured (a local process over stdio, or a remote one
// over Streamable HTTP or the older SSE transport) and offers their tools to
// the agent loop as ordinary tools.
//
// Two protocol eras are spoken. The legacy one (2025-11-25 and earlier) opens
// with an initialize handshake and, over HTTP, a session. The modern one
// (2026-07-28) has neither: every request carries its version and
// capabilities. A server is tried as legacy first, since that is what nearly
// every server speaks, and as modern when initialize is refused.
package mcp

import (
	"encoding/json"
	"fmt"
)

// message is one JSON-RPC 2.0 message of any kind.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (m *message) isRequest() bool  { return m.Method != "" && len(m.ID) > 0 }
func (m *message) isNotify() bool   { return m.Method != "" && len(m.ID) == 0 }
func (m *message) isResponse() bool { return m.Method == "" && len(m.ID) > 0 }

// rpcError is a JSON-RPC error object. It is also what a failed call returns,
// so a caller can tell the server said no (and why) from a broken connection.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("%s (code %d)", e.Message, e.Code) }

const (
	errMethodNotFound = -32601
	errInvalidParams  = -32602
)

func newRequest(id int64, method string, params any) (*message, error) {
	m := &message{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprint(id)), Method: method}
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		m.Params = b
	}
	return m, nil
}

func newNotification(method string, params any) *message {
	m := &message{JSONRPC: "2.0", Method: method}
	if params != nil {
		m.Params, _ = json.Marshal(params)
	}
	return m
}

func replyError(req *message, code int, msg string) *message {
	return &message{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: code, Message: msg}}
}

func replyResult(req *message, result any) *message {
	b, _ := json.Marshal(result)
	return &message{JSONRPC: "2.0", ID: req.ID, Result: b}
}

// parseMessages decodes one JSON-RPC message or a batch of them.
func parseMessages(data []byte) ([]*message, error) {
	data = trimSpace(data)
	if len(data) == 0 {
		return nil, fmt.Errorf("empty message")
	}
	if data[0] == '[' {
		var batch []*message
		if err := json.Unmarshal(data, &batch); err != nil {
			return nil, err
		}
		// A null entry is not a message; dropping it here keeps every reader
		// from dereferencing it.
		msgs := batch[:0]
		for _, m := range batch {
			if m != nil {
				msgs = append(msgs, m)
			}
		}
		return msgs, nil
	}
	var m message
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return []*message{&m}, nil
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\n' || b[0] == '\r' || b[0] == '\t') {
		b = b[1:]
	}
	for len(b) > 0 && (b[len(b)-1] == ' ' || b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == '\t') {
		b = b[:len(b)-1]
	}
	return b
}

// idKey is the map key for a message id (numbers and strings both occur).
func idKey(id json.RawMessage) string { return string(id) }
