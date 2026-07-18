// Package mcpserver exposes the awsmux engine to agents over the Model
// Context Protocol (stdio transport). This is the primary interface of
// awsmux: agents get structured tools with a hard plan/approve/execute
// boundary instead of a raw shell.
//
// Protocol notes (hand-rolled on purpose, zero deps):
//   - JSON-RPC 2.0, one message per line on stdin/stdout (ndjson framing).
//   - Handle: initialize, notifications/initialized (ignore), ping,
//     tools/list, tools/call. Unknown methods with an id get error -32601;
//     notifications are silently dropped.
//   - initialize: echo the client's protocolVersion, capabilities
//     {"tools":{}}, serverInfo {"name":"awsmux","version":Version}.
//   - tools/call result: {"content":[{"type":"text","text":<pretty JSON
//     payload>}],"isError":false}. Domain failures (bad selector, approval
//     missing) return isError true with a JSON {"error": ...} payload, NOT
//     a JSON-RPC error; protocol errors use JSON-RPC errors.
//   - Log to stderr only; stdout carries protocol frames exclusively.
//
// Tools (schemas in tools.go):
//
//	list_aws_targets   {profiles?, exclude?, regions?, preflight?=true,
//	                    dedupe?} -> {targets:[...], count}
//	plan_aws_operation {service, operation, args?, profiles?, exclude?,
//	                    regions?, dedupe?, target_ids?} ->
//	                    the plan (id, classification, requires_approval,
//	                    target_count, hash, expires_at, approval_hint)
//	execute_aws_plan   {plan_id, approval_token?, concurrency?, timeout_s?,
//	                    max_errors?, wait?=true} ->
//	                    wait: the full execution with summary;
//	                    else {execution_id, status:"running"}
//	get_aws_execution  {execution_id} -> execution (running ones come from
//	                    the in-process registry, finished from the store)
//	cancel_aws_execution {execution_id} -> {cancelled: bool}
//
// Rules the implementation must enforce:
//   - plan_aws_operation with target_ids re-resolves and preflights those
//     targets; selectors and target_ids compose (target_ids filter wins).
//   - execute_aws_plan goes through core.CheckApproval. There is NO code
//     path that executes a non-read-only plan without a valid token. The
//     approval_hint in plan responses tells the agent to ask a human to run
//     `awsmux approve <plan-id>`.
//   - Executed plans get Status PlanExecuted + ExecutionID and are saved,
//     so a plan cannot be executed twice.
//   - Async executions run in a goroutine, live in a mutex-guarded
//     registry keyed by execution ID with their cancel func, and are saved
//     to the store when done.
package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
)

// Version is reported in serverInfo.
const Version = "0.1.0"

// server bundles per-process MCP state: the frame writer, the async
// execution registry, and the mutex serializing the plan approval gate.
type server struct {
	outMu  sync.Mutex
	out    io.Writer
	logger *log.Logger
	reg    *registry
	// execMu serializes execute_aws_plan's load/check/mark-executed phase
	// so a plan cannot pass the approval gate twice concurrently.
	execMu sync.Mutex
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Serve runs the MCP server on stdin/stdout until EOF or ctx cancel.
func Serve(ctx context.Context) error {
	s := &server{
		out:    os.Stdout,
		logger: log.New(os.Stderr, "awsmux-mcp ", log.LstdFlags),
		reg:    newRegistry(),
	}
	s.logf("awsmux mcp server ready")

	lines := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := append([]byte(nil), sc.Bytes()...)
			select {
			case lines <- line:
			case <-ctx.Done():
				return
			}
		}
		readErr <- sc.Err()
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-readErr:
			if err != nil {
				return fmt.Errorf("read stdin: %w", err)
			}
			return nil // EOF
		case line := <-lines:
			s.handleLine(ctx, line)
		}
	}
}

func (s *server) logf(format string, args ...any) {
	s.logger.Printf(format, args...)
}

func (s *server) handleLine(ctx context.Context, line []byte) {
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		s.replyError(nil, -32700, "parse error: invalid JSON")
		return
	}
	if req.ID == nil {
		// Missing id means notification (notifications/initialized and
		// friends): nothing to answer.
		return
	}
	switch req.Method {
	case "initialize":
		s.logf("initialize")
		s.reply(req.ID, initializeResult(req.Params))
	case "notifications/initialized":
		// Normally a notification; if a client sends it with an id, an
		// empty result is the harmless answer.
		s.reply(req.ID, map[string]any{})
	case "ping":
		s.reply(req.ID, map[string]any{})
	case "tools/list":
		s.logf("tools/list")
		s.reply(req.ID, map[string]any{"tools": tools})
	case "tools/call":
		s.handleToolsCall(ctx, req.ID, req.Params)
	default:
		s.replyError(req.ID, -32601, fmt.Sprintf("method not found: %s", req.Method))
	}
}

func initializeResult(params json.RawMessage) map[string]any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	if p.ProtocolVersion == "" {
		p.ProtocolVersion = "2025-06-18"
	}
	return map[string]any{
		"protocolVersion": p.ProtocolVersion,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "awsmux", "version": Version},
	}
}

func (s *server) handleToolsCall(ctx context.Context, id, params json.RawMessage) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Name == "" {
		s.replyError(id, -32602, "invalid tools/call params: need {name, arguments}")
		return
	}
	def := findTool(p.Name)
	if def == nil {
		s.replyError(id, -32602, fmt.Sprintf("unknown tool: %s", p.Name))
		return
	}
	payload, err := def.handler(s, ctx, p.Arguments)
	if err != nil {
		s.logf("tools/call %s: error: %v", p.Name, err)
		s.reply(id, toolErrorResult(err))
		return
	}
	s.logf("tools/call %s: ok", p.Name)
	res, merr := toolSuccessResult(payload)
	if merr != nil {
		s.replyError(id, -32603, merr.Error())
		return
	}
	s.reply(id, res)
}

// toolSuccessResult wraps a payload in the MCP content envelope, with
// structuredContent mirroring the JSON for modern clients.
func toolSuccessResult(payload any) (map[string]any, error) {
	pretty, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode tool result: %w", err)
	}
	return map[string]any{
		"content":           []map[string]any{{"type": "text", "text": string(pretty)}},
		"structuredContent": payload,
		"isError":           false,
	}, nil
}

// toolErrorResult wraps a domain error as an isError tool result, not a
// JSON-RPC error, so the agent sees and can react to it.
func toolErrorResult(err error) map[string]any {
	pretty, merr := json.MarshalIndent(map[string]string{"error": err.Error()}, "", "  ")
	if merr != nil {
		pretty = []byte(`{"error":"internal encoding failure"}`)
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(pretty)}},
		"isError": true,
	}
}

func (s *server) reply(id json.RawMessage, result any) {
	s.send(rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *server) replyError(id json.RawMessage, code int, msg string) {
	s.send(rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

// send marshals one response frame per line; a nil ID marshals as null,
// which is exactly what a parse-error response needs.
func (s *server) send(resp rpcResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		s.logf("marshal response: %v", err)
		return
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	if _, err := s.out.Write(append(data, '\n')); err != nil {
		s.logf("write response: %v", err)
	}
}
