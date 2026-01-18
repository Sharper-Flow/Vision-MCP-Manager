// Package bridge provides the stdio-to-HTTP bridge for MCP servers.
// It enables multi-client access to stdio-based MCP servers over HTTP.
package bridge

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
)

// JSON-RPC 2.0 types for MCP communication.

// Request represents a JSON-RPC 2.0 request.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      any             `json:"id,omitempty"`
}

// Response represents a JSON-RPC 2.0 response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
	ID      any             `json:"id"`
}

// Notification represents a JSON-RPC 2.0 notification (no ID).
type Notification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Error represents a JSON-RPC 2.0 error object.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Standard JSON-RPC error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// Error implements the error interface.
func (e *Error) Error() string {
	if e.Data != nil {
		return fmt.Sprintf("JSON-RPC error %d: %s (data: %s)", e.Code, e.Message, string(e.Data))
	}
	return fmt.Sprintf("JSON-RPC error %d: %s", e.Code, e.Message)
}

// NewError creates a new JSON-RPC error.
func NewError(code int, message string) *Error {
	return &Error{Code: code, Message: message}
}

// NewErrorWithData creates a new JSON-RPC error with additional data.
func NewErrorWithData(code int, message string, data any) *Error {
	dataBytes, _ := json.Marshal(data)
	return &Error{Code: code, Message: message, Data: dataBytes}
}

// NewRequest creates a new JSON-RPC request with the given method and params.
func NewRequest(id any, method string, params any) (*Request, error) {
	var paramsBytes json.RawMessage
	if params != nil {
		var err error
		paramsBytes, err = json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal params: %w", err)
		}
	}

	return &Request{
		JSONRPC: "2.0",
		Method:  method,
		Params:  paramsBytes,
		ID:      id,
	}, nil
}

// NewResponse creates a successful JSON-RPC response.
func NewResponse(id any, result any) (*Response, error) {
	resultBytes, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal result: %w", err)
	}

	return &Response{
		JSONRPC: "2.0",
		Result:  resultBytes,
		ID:      id,
	}, nil
}

// NewErrorResponse creates a JSON-RPC error response.
func NewErrorResponse(id any, err *Error) *Response {
	return &Response{
		JSONRPC: "2.0",
		Error:   err,
		ID:      id,
	}
}

// IsRequest returns true if the message is a request (has an ID).
func (r *Request) IsRequest() bool {
	return r.ID != nil
}

// IsNotification returns true if the message is a notification (no ID).
func (r *Request) IsNotification() bool {
	return r.ID == nil
}

// ParseMessage parses a JSON-RPC message and returns either a Request or Response.
func ParseMessage(data []byte) (any, error) {
	// Try to detect if it's a response (has result or error field)
	var probe struct {
		Result json.RawMessage `json:"result"`
		Error  *Error          `json:"error"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("failed to parse message: %w", err)
	}

	// If it has a result or error, it's a response
	if probe.Result != nil || probe.Error != nil {
		var resp Response
		if err := json.Unmarshal(data, &resp); err != nil {
			return nil, fmt.Errorf("failed to parse response: %w", err)
		}
		return &resp, nil
	}

	// Otherwise it's a request/notification
	var req Request
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, fmt.Errorf("failed to parse request: %w", err)
	}
	return &req, nil
}

// IDGenerator generates unique request IDs.
type IDGenerator struct {
	counter atomic.Int64
}

// Next returns the next unique ID.
func (g *IDGenerator) Next() int64 {
	return g.counter.Add(1)
}

// NewIDGenerator creates a new ID generator.
func NewIDGenerator() *IDGenerator {
	return &IDGenerator{}
}

// ExtractID extracts the ID from a JSON-RPC message as a string.
// Returns empty string if no ID is present.
func ExtractID(id any) string {
	if id == nil {
		return ""
	}
	switch v := id.(type) {
	case string:
		return v
	case float64:
		return fmt.Sprintf("%.0f", v)
	case int:
		return fmt.Sprintf("%d", v)
	case int64:
		return fmt.Sprintf("%d", v)
	default:
		return fmt.Sprintf("%v", v)
	}
}
