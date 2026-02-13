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
	"sync"

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
			// OPTIONS handled by cors wrapper
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// isNotificationOrResponseOnly checks whether a JSON-RPC payload contains no request objects.
// If payload has any item with "method" and "id", treat as request-containing.
func isNotificationOrResponseOnly(body []byte) bool {
	var raw any
	if err := json.Unmarshal(body, &raw); err != nil {
		return false
	}

	checkObj := func(m map[string]any) (hasRequest bool) {
		method, hasMethod := m["method"]
		_, hasID := m["id"]

		// JSON-RPC request: has method + has id
		if hasMethod {
			_, _ = method.(string)
			if hasID {
				return true
			}
		}
		return false
	}

	switch v := raw.(type) {
	case map[string]any:
		// single message
		return !checkObj(v)
	case []any:
		// batch: if any request exists => not notification/response-only
		for _, it := range v {
			m, ok := it.(map[string]any)
			if !ok {
				return false
			}
			if checkObj(m) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// handleMCPPost processes incoming JSON-RPC requests over Streamable HTTP.
func handleMCPPost(ctx context.Context, w http.ResponseWriter, req *http.Request, ss *StreamableSessions, mcpsrv *MCPServer) {
	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		slog.Error("streamable read body", slog.Any("err", err))
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	defer req.Body.Close()

	mcpreq, err := mcpsrv.Decode(bytes.NewReader(bodyBytes))
	if err != nil {
		slog.Error("streamable decode", slog.Any("err", err))

		// Per spec: responses/notifications-only can be 202 Accepted
		if isNotificationOrResponseOnly(bodyBytes) {
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

		// Minimal safe behavior: always JSON for initialize response
		writeJSON(w, rpc.NewResponse(mcp.InitializeResponse{
			ProtocolVersion: mcp.Version,
			ServerInfo: mcp.Info{
				Name:    mcpsrv.Name,
				Version: mcpsrv.Version,
			},
			Capabilities: mcp.Capabilities{"tools": mcp.Capability{}},
		}, r.Request.Id))

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

