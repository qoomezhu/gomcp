// Copyright 2025 Lightpanda (Selecy SAS)
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-contrib/sse"
	"github.com/google/uuid"
	"github.com/lightpanda-io/gomcp/mcp"
	"github.com/lightpanda-io/gomcp/rpc"
)

// StreamableSession holds state for a single Streamable HTTP session.
type StreamableSession struct {
	id      string
	mcpconn *MCPConn
	mu      sync.Mutex
}

// StreamableSessions manages all active Streamable HTTP sessions.
type StreamableSessions struct {
	sync.RWMutex
	sessions map[string]*StreamableSession
	mcpsrv   *MCPServer
}

func NewStreamableSessions(mcpsrv *MCPServer) *StreamableSessions {
	return &StreamableSessions{
		sessions: make(map[string]*StreamableSession),
		mcpsrv:   mcpsrv,
	}
}

func (ss *StreamableSessions) Create() *StreamableSession {
	s := &StreamableSession{
		id:      uuid.New().String(),
		mcpconn: ss.mcpsrv.NewConn(),
	}
	ss.Lock()
	ss.sessions[s.id] = s
	ss.Unlock()
	return s
}

func (ss *StreamableSessions) Get(id string) (*StreamableSession, bool) {
	ss.RLock()
	s, ok := ss.sessions[id]
	ss.RUnlock()
	return s, ok
}

func (ss *StreamableSessions) Remove(id string) {
	ss.Lock()
	if s, ok := ss.sessions[id]; ok {
		s.mcpconn.Close()
		delete(ss.sessions, id)
	}
	ss.Unlock()
}

// handleMCP is the main handler for the /mcp Streamable HTTP endpoint.
// It routes requests by HTTP method to the appropriate sub-handler.
func handleMCP(ctx context.Context, ss *StreamableSessions, mcpsrv *MCPServer) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case http.MethodPost:
			handleMCPPost(ctx, w, req, ss, mcpsrv)
		case http.MethodGet:
			handleMCPGet(ctx, w, req, ss)
		case http.MethodDelete:
			handleMCPDelete(w, req, ss)
		default:
			// OPTIONS is handled by cors wrapper
		}
	}
}

// acceptSSE checks if the client accepts SSE streaming response.
func acceptSSE(req *http.Request) bool {
	accept := req.Header.Get("Accept")
	if accept == "" {
		return false
	}
	// Check for text/event-stream in Accept header
	for _, v := range strings.Split(accept, ",") {
		v = strings.TrimSpace(v)
		if strings.HasPrefix(v, "text/event-stream") {
			return true
		}
	}
	return false
}

// isNotificationOnly checks if the JSON-RPC message is a notification or response only
// (i.e., no "id" field, or only contains "result"/"error" fields).
// Returns true if the server should respond with 202 Accepted.
func isNotificationOnly(bodyBytes []byte) bool {
	// Try to decode as array first (batch)
	var batch []json.RawMessage
	if err := json.Unmarshal(bodyBytes, &batch); err == nil {
		// It's a batch - check each item
		for _, item := range batch {
			if !isNotificationOrResponse(item) {
				return false
			}
		}
		return true
	}

	// Single message
	return isNotificationOrResponse(bodyBytes)
}

// isNotificationOrResponse checks if a single JSON-RPC message is a notification or response.
func isNotificationOrResponse(raw json.RawMessage) bool {
	var msg struct {
		Id     any `json:"id"`
		Result any `json:"result"`
		Error  any `json:"error"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return false
	}
	// If it has an id, it's a request (not notification-only)
	// If it has result or error (and no id), it's a response
	return msg.Id == nil || msg.Result != nil || msg.Error != nil
}

// handleMCPPost processes incoming JSON-RPC requests over Streamable HTTP.
// Per MCP 2025-03-26 spec:
// - POST body can be a single request/notification/response or a batch
// - For requests: server returns SSE stream or JSON response
// - For notifications/responses only: server returns 202 Accepted
func handleMCPPost(ctx context.Context, w http.ResponseWriter, req *http.Request, ss *StreamableSessions, mcpsrv *MCPServer) {
	// Read and buffer the body for potential re-reading
	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		slog.Error("streamable read body", slog.Any("err", err))
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	defer req.Body.Close()

	// Decode the JSON-RPC request from the body.
	mcpreq, err := mcpsrv.Decode(bytes.NewReader(bodyBytes))
	if err != nil {
		slog.Error("streamable decode", slog.Any("err", err))
		// Check if this is a notification/response only - return 202 Accepted
		if isNotificationOnly(bodyBytes) {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	switch r := mcpreq.(type) {
	case mcp.InitializeRequest:
		// Create a new session on initialize.
		session := ss.Create()
		slog.Debug("streamable init", slog.String("session", session.id))
		w.Header().Set("Mcp-Session-Id", session.id)

		// Check if client wants SSE stream
		if acceptSSE(req) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.Header().Set("Mcp-Session-Id", session.id)

			f, ok := w.(http.Flusher)
			if !ok {
				http.Error(w, "streaming not supported", http.StatusInternalServerError)
				return
			}

			// Send initialize response as SSE message event
			err := sse.Encode(w, sse.Event{
				Event: "message",
				Data:  rpc.NewResponse(mcp.InitializeResponse{
					ProtocolVersion: mcp.Version,
					ServerInfo: mcp.Info{
						Name:    mcpsrv.Name,
						Version: mcpsrv.Version,
					},
					Capabilities: mcp.Capabilities{"tools": mcp.Capability{}},
				}, r.Request.Id),
			})
			if err != nil {
				slog.Error("sse encode", slog.Any("err", err))
				return
			}
			f.Flush()
			// Keep connection open for server-to-client messages
			select {
			case <-req.Context().Done():
			case <-ctx.Done():
			}
		} else {
			// Return JSON response
			writeJSON(w, rpc.NewResponse(mcp.InitializeResponse{
				ProtocolVersion: mcp.Version,
				ServerInfo: mcp.Info{
					Name:    mcpsrv.Name,
					Version: mcpsrv.Version,
				},
				Capabilities: mcp.Capabilities{"tools": mcp.Capability{}},
			}, r.Request.Id))
		}

	case mcp.NotificationsInitializedRequest:
		setSessionHeader(w, req)
		w.WriteHeader(http.StatusAccepted)

	case mcp.NotificationsCancelledRequest:
		setSessionHeader(w, req)
		w.WriteHeader(http.StatusAccepted)

	case mcp.ResourcesListRequest:
		session, ok := requireSession(w, req, ss)
		if !ok {
			return
		}
		w.Header().Set("Mcp-Session-Id", session.id)
		writeJSON(w, rpc.NewResponse(struct{}{}, r.Id))

	case mcp.PromptsListRequest:
		session, ok := requireSession(w, req, ss)
		if !ok {
			return
		}
		w.Header().Set("Mcp-Session-Id", session.id)
		writeJSON(w, rpc.NewResponse(struct{}{}, r.Id))

	case mcp.ToolsListRequest:
		session, ok := requireSession(w, req, ss)
		if !ok {
			return
		}
		w.Header().Set("Mcp-Session-Id", session.id)
		writeJSON(w, rpc.NewResponse(mcp.ToolsListResponse{
			Tools: mcpsrv.ListTools(),
		}, r.Id))

	case mcp.ToolsCallRequest:
		session, ok := requireSession(w, req, ss)
		if !ok {
			return
		}
		// Lock per session to serialize browser access.
		session.mu.Lock()
		defer session.mu.Unlock()

		slog.Debug("streamable tool call", slog.String("name", r.Params.Name))

		// CallTool blocks until the browser operation completes.
		res, err := mcpsrv.CallTool(ctx, session.mcpconn, r)
		w.Header().Set("Mcp-Session-Id", session.id)
		if err != nil {
			slog.Error("streamable tool error", slog.String("name", r.Params.Name), slog.Any("err", err))
			writeJSON(w, rpc.NewResponse(mcp.ToolsCallResponse{
				IsError: true,
				Content: []mcp.ToolsCallContent{{Type: "text", Text: err.Error()}},
			}, r.Id))
			return
		}
		writeJSON(w, rpc.NewResponse(mcp.ToolsCallResponse{
			Content: []mcp.ToolsCallContent{{Type: "text", Text: res}},
		}, r.Id))

	default:
		http.Error(w, "unsupported method", http.StatusBadRequest)
	}
}

// handleMCPGet keeps an SSE connection open for server-initiated notifications.
func handleMCPGet(ctx context.Context, w http.ResponseWriter, req *http.Request, ss *StreamableSessions) {
	sessionId := req.Header.Get("Mcp-Session-Id")
	if sessionId == "" {
		http.Error(w, "Mcp-Session-Id required", http.StatusBadRequest)
		return
	}
	if _, ok := ss.Get(sessionId); !ok {
		http.Error(w, "invalid session", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Mcp-Session-Id", sessionId)

	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// Block until client disconnects or server shuts down.
	select {
	case <-req.Context().Done():
	case <-ctx.Done():
	}
}

// handleMCPDelete closes and removes a session.
func handleMCPDelete(w http.ResponseWriter, req *http.Request, ss *StreamableSessions) {
	sessionId := req.Header.Get("Mcp-Session-Id")
	if sessionId == "" {
		http.Error(w, "Mcp-Session-Id required", http.StatusBadRequest)
		return
	}
	ss.Remove(sessionId)
	slog.Debug("streamable session removed", slog.String("id", sessionId))
	w.WriteHeader(http.StatusNoContent)
}

// requireSession extracts and validates the session from Mcp-Session-Id header.
func requireSession(w http.ResponseWriter, req *http.Request, ss *StreamableSessions) (*StreamableSession, bool) {
	sessionId := req.Header.Get("Mcp-Session-Id")
	if sessionId == "" {
		http.Error(w, "Mcp-Session-Id required", http.StatusBadRequest)
		return nil, false
	}
	session, ok := ss.Get(sessionId)
	if !ok {
		http.Error(w, "invalid session", http.StatusNotFound)
		return nil, false
	}
	return session, true
}

// setSessionHeader echoes the incoming Mcp-Session-Id back in the response.
func setSessionHeader(w http.ResponseWriter, req *http.Request) {
	if id := req.Header.Get("Mcp-Session-Id"); id != "" {
		w.Header().Set("Mcp-Session-Id", id)
	}
}

// writeJSON writes a JSON response with proper Content-Type.
func writeJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		slog.Error("json encode", slog.Any("err", err))
	}
}
