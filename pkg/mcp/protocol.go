package mcp

import (
	"encoding/json"
	"fmt"
)

const MCPVersion = "2024-11-05"

type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type JSONRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *JSONRPCError   `json:"error,omitempty"`
}

type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type InitializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ClientInfo     struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"clientInfo"`
}

type InitializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    ServerCaps     `json:"capabilities"`
	ServerInfo      ServerInfo     `json:"serverInfo"`
	Instructions   string         `json:"instructions"`
}

type ServerCaps struct {
	Tools struct {
		ListChanged bool `json:"listChanged"`
	} `json:"tools"`
	Resources struct {
		Subscribe   bool `json:"subscribe"`
		ListChanged bool `json:"listChanged"`
	} `json:"resources"`
}

type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description"`
	MIMEType    string `json:"mimeType,omitempty"`
}

func NewError(code int, message string) *JSONRPCError {
	return &JSONRPCError{Code: code, Message: message}
}

func InternalError(err error) *JSONRPCError {
	return &JSONRPCError{Code: -32603, Message: err.Error()}
}

func InvalidParams(msg string) *JSONRPCError {
	return &JSONRPCError{Code: -32602, Message: msg}
}

func NewResponse(id json.RawMessage, result any) JSONRPCResponse {
	return JSONRPCResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func NewErrorResponse(id json.RawMessage, err *JSONRPCError) JSONRPCResponse {
	return JSONRPCResponse{JSONRPC: "2.0", ID: id, Error: err}
}

func ParseRequest(data []byte) (*JSONRPCRequest, error) {
	var req JSONRPCRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, fmt.Errorf("parse request: %w", err)
	}
	if req.JSONRPC != "2.0" {
		return nil, fmt.Errorf("unsupported jsonrpc version: %s", req.JSONRPC)
	}
	return &req, nil
}

func MarshalResponse(resp JSONRPCResponse) []byte {
	data, err := json.Marshal(resp)
	if err != nil {
		errResp := JSONRPCResponse{
			JSONRPC: "2.0",
			Error:   InternalError(fmt.Errorf("marshal response: %w", err)),
		}
		data, _ = json.Marshal(errResp)
	}
	return data
}
