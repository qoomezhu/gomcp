package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	md "github.com/JohannesKaufmann/html-to-markdown"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/chromedp"
	"github.com/lightpanda-io/gomcp/mcp"
	"github.com/lightpanda-io/gomcp/rpc"
)

const functionName = "gomcp netlify lite"

type SessionState struct {
	CurrentURL string `json:"current_url,omitempty"`
	IssuedAt   int64  `json:"issued_at"`
}

func (s SessionState) normalize() SessionState {
	if s.IssuedAt == 0 {
		s.IssuedAt = time.Now().Unix()
	}
	return s
}

func encodeSession(state SessionState, secret string) (string, error) {
	state = state.normalize()
	payload, err := json.Marshal(state)
	if err != nil {
		return "", err
	}

	mac := hmac.New(sha256.New, []byte(secret))
	if _, err := mac.Write(payload); err != nil {
		return "", err
	}

	sig := mac.Sum(nil)
	buf := make([]byte, 0, len(sig)+len(payload))
	buf = append(buf, sig...)
	buf = append(buf, payload...)

	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func decodeSession(token, secret string) (SessionState, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return SessionState{}, nil
	}

	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return SessionState{}, err
	}
	if len(raw) < sha256.Size {
		return SessionState{}, errors.New("invalid session token")
	}

	sig := raw[:sha256.Size]
	payload := raw[sha256.Size:]

	mac := hmac.New(sha256.New, []byte(secret))
	if _, err := mac.Write(payload); err != nil {
		return SessionState{}, err
	}
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return SessionState{}, errors.New("invalid session token")
	}

	var state SessionState
	if err := json.Unmarshal(payload, &state); err != nil {
		return SessionState{}, err
	}

	return state, nil
}

func headerLookup(headers map[string]string, name string) string {
	if len(headers) == 0 {
		return ""
	}

	if v, ok := headers[name]; ok {
		return v
	}

	name = strings.ToLower(name)
	for k, v := range headers {
		if strings.ToLower(k) == name {
			return v
		}
	}

	return ""
}

func requestBody(req events.APIGatewayProxyRequest) ([]byte, error) {
	if req.Body == "" {
		return nil, errors.New("empty request body")
	}
	if req.IsBase64Encoded {
		return base64.StdEncoding.DecodeString(req.Body)
	}
	return []byte(req.Body), nil
}

func responseHeaders() map[string]string {
	return map[string]string{
		"Access-Control-Allow-Origin":  "*",
		"Access-Control-Allow-Headers": "content-type,accept,authorization,mcp-session-id,mcp-protocol-version",
		"Access-Control-Allow-Methods": "POST,GET,DELETE,OPTIONS",
		"Access-Control-Expose-Headers": "Mcp-Session-Id",
	}
}

func buildResponse(status int, body any, sessionID string) (events.APIGatewayProxyResponse, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return events.APIGatewayProxyResponse{}, err
	}

	headers := responseHeaders()
	headers["Content-Type"] = "application/json"
	if sessionID != "" {
		headers["Mcp-Session-Id"] = sessionID
	}

	return events.APIGatewayProxyResponse{
		StatusCode: status,
		Headers:    headers,
		Body:       string(raw),
	}, nil
}

func rpcResponse(body any, sessionID string) (events.APIGatewayProxyResponse, error) {
	return buildResponse(http.StatusOK, body, sessionID)
}

func rpcErrorResponse(id any, code int, message, sessionID string) (events.APIGatewayProxyResponse, error) {
	return buildResponse(http.StatusOK, rpc.NewErrorResponse(id, code, message), sessionID)
}

func emptyResponse(status int) events.APIGatewayProxyResponse {
	return events.APIGatewayProxyResponse{
		StatusCode: status,
		Headers:    responseHeaders(),
	}
}

func withRemotePage(ctx context.Context, cdpURL string, fn func(context.Context) (string, error)) (string, error) {
	allocCtx, allocCancel := chromedp.NewRemoteAllocator(ctx, cdpURL, chromedp.NoModifyURL)
	defer allocCancel()

	tabCtx, tabCancel := chromedp.NewContext(allocCtx)
	defer tabCancel()

	if err := chromedp.Run(tabCtx); err != nil {
		return "", fmt.Errorf("new tab: %w", err)
	}

	return fn(tabCtx)
}

func gotoURL(ctx context.Context, cdpURL, target string) (string, error) {
	return withRemotePage(ctx, cdpURL, func(tabCtx context.Context) (string, error) {
		if err := chromedp.Run(tabCtx, chromedp.Navigate(target)); err != nil {
			return "", fmt.Errorf("navigate %s: %w", target, err)
		}

		return fmt.Sprintf("The browser correctly navigated to '%s', the page is loaded in the context of the browser and can be used.", target), nil
	})
}

func markdownFromURL(ctx context.Context, cdpURL, target string) (string, error) {
	return withRemotePage(ctx, cdpURL, func(tabCtx context.Context) (string, error) {
		if err := chromedp.Run(tabCtx, chromedp.Navigate(target)); err != nil {
			return "", fmt.Errorf("navigate %s: %w", target, err)
		}

		var html string
		if err := chromedp.Run(tabCtx, chromedp.OuterHTML("html", &html)); err != nil {
			return "", fmt.Errorf("outerHTML: %w", err)
		}

		converter := md.NewConverter("", true, nil)
		content, err := converter.ConvertString(html)
		if err != nil {
			return "", fmt.Errorf("convert markdown: %w", err)
		}

		return content, nil
	})
}

func linksFromURL(ctx context.Context, cdpURL, target string) (string, error) {
	return withRemotePage(ctx, cdpURL, func(tabCtx context.Context) (string, error) {
		if err := chromedp.Run(tabCtx, chromedp.Navigate(target)); err != nil {
			return "", fmt.Errorf("navigate %s: %w", target, err)
		}

		var nodes []*cdp.Node
		if err := chromedp.Run(tabCtx, chromedp.Nodes(`a[href]`, &nodes)); err != nil {
			return "", fmt.Errorf("get links: %w", err)
		}

		baseURL, _ := url.Parse(target)
		links := make([]string, 0, len(nodes))
		for _, node := range nodes {
			href, ok := node.Attribute("href")
			if !ok || href == "" {
				continue
			}
			if baseURL != nil {
				if ref, err := url.Parse(href); err == nil {
					href = baseURL.ResolveReference(ref).String()
				}
			}
			links = append(links, href)
		}

		return strings.Join(links, "\n"), nil
	})
}

func tools() []mcp.Tool {
	return []mcp.Tool{
		{
			Name:        "goto",
			Description: "Navigate a remote CDP browser to a specified URL and store the URL in the Netlify session token so the page can be reopened later.",
			InputSchema: mcp.NewSchemaObject(mcp.Properties{
				"url": mcp.NewSchemaString("The URL to navigate to, must be a valid URL."),
			}, "url"),
		},
		{
			Name:        "search",
			Description: "Use DuckDuckGo to search for text in the remote browser and store the search URL in the Netlify session token.",
			InputSchema: mcp.NewSchemaObject(mcp.Properties{
				"text": mcp.NewSchemaString("The text to search for, must be a valid search query."),
			}, "text"),
		},
		{
			Name:        "markdown",
			Description: "Reopen the current session URL in the remote browser and return the page content in Markdown format.",
			InputSchema: mcp.NewSchemaObject(mcp.Properties{}),
		},
		{
			Name:        "links",
			Description: "Reopen the current session URL in the remote browser and extract all links in the page.",
			InputSchema: mcp.NewSchemaObject(mcp.Properties{}),
		},
		{
			Name:        "over",
			Description: "Used to indicate that the task is over and give the final answer if there is any. This is the last tool to be called in a task.",
			InputSchema: mcp.NewSchemaObject(mcp.Properties{
				"result": mcp.NewSchemaString("The final result of the task."),
			}),
		},
	}
}

var ErrNoTool = errors.New("no tool found")

func callTool(ctx context.Context, cdpURL string, state SessionState, req mcp.ToolsCallRequest) (string, SessionState, error) {
	toolCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	switch req.Params.Name {
	case "goto":
		var args struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return "", state, fmt.Errorf("args decode: %w", err)
		}
		if args.URL == "" {
			return "", state, errors.New("no url")
		}
		res, err := gotoURL(toolCtx, cdpURL, args.URL)
		if err != nil {
			return "", state, err
		}
		state.CurrentURL = args.URL
		return res, state, nil

	case "search":
		var args struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return "", state, fmt.Errorf("args decode: %w", err)
		}
		if args.Text == "" {
			return "", state, errors.New("no text")
		}
		searchURL := "https://duckduckgo.com/?q=" + url.QueryEscape(args.Text)
		res, err := gotoURL(toolCtx, cdpURL, searchURL)
		if err != nil {
			return "", state, err
		}
		state.CurrentURL = searchURL
		return res, state, nil

	case "markdown":
		if state.CurrentURL == "" {
			return "", state, errors.New("no browser page, try to use goto first")
		}
		res, err := markdownFromURL(toolCtx, cdpURL, state.CurrentURL)
		return res, state, err

	case "links":
		if state.CurrentURL == "" {
			return "", state, errors.New("no browser page, try to use goto first")
		}
		res, err := linksFromURL(toolCtx, cdpURL, state.CurrentURL)
		return res, state, err

	case "over":
		var args struct {
			Result string `json:"result"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return "", state, fmt.Errorf("args decode: %w", err)
		}
		return args.Result, state, nil
	}

	return "", state, ErrNoTool
}

func sessionFromHeader(req events.APIGatewayProxyRequest, secret string) (SessionState, error) {
	return decodeSession(headerLookup(req.Headers, "Mcp-Session-Id"), secret)
}

func handleNoopGet(req events.APIGatewayProxyRequest) events.APIGatewayProxyResponse {
	headers := responseHeaders()
	if id := headerLookup(req.Headers, "Mcp-Session-Id"); id != "" {
		headers["Mcp-Session-Id"] = id
	}
	return events.APIGatewayProxyResponse{StatusCode: http.StatusNoContent, Headers: headers}
}

func handler(ctx context.Context, req events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	switch req.HTTPMethod {
	case http.MethodOptions:
		return emptyResponse(http.StatusNoContent), nil
	case http.MethodGet:
		return handleNoopGet(req), nil
	case http.MethodDelete:
		return emptyResponse(http.StatusNoContent), nil
	case http.MethodPost:
		// continue below
	default:
		return events.APIGatewayProxyResponse{
			StatusCode: http.StatusMethodNotAllowed,
			Headers:    responseHeaders(),
			Body:       "method not allowed",
		}, nil
	}

	cdpURL := os.Getenv("MCP_CDP")
	if cdpURL == "" {
		return events.APIGatewayProxyResponse{
			StatusCode: http.StatusInternalServerError,
			Headers:    responseHeaders(),
			Body:       "MCP_CDP is required",
		}, nil
	}
	secret := os.Getenv("SESSION_SECRET")
	if secret == "" {
		return events.APIGatewayProxyResponse{
			StatusCode: http.StatusInternalServerError,
			Headers:    responseHeaders(),
			Body:       "SESSION_SECRET is required",
		}, nil
	}

	contentType := headerLookup(req.Headers, "Content-Type")
	if contentType != "" && !strings.Contains(strings.ToLower(contentType), "application/json") {
		return events.APIGatewayProxyResponse{
			StatusCode: http.StatusUnsupportedMediaType,
			Headers:    responseHeaders(),
			Body:       "Content-Type must be application/json",
		}, nil
	}

	body, err := requestBody(req)
	if err != nil {
		return rpcErrorResponse(nil, rpc.ParseError, err.Error(), "")
	}

	var rpcReq rpc.Request
	if err := json.Unmarshal(body, &rpcReq); err != nil {
		return rpcErrorResponse(nil, rpc.ParseError, "invalid JSON-RPC request", "")
	}
	if err := rpcReq.Validate(); err != nil {
		return rpcErrorResponse(rpcReq.Id, rpc.InvalidRequest, err.Error(), "")
	}

	mcpReq, err := mcp.Decode(rpcReq)
	if err != nil {
		return rpcErrorResponse(rpcReq.Id, rpc.MethodNotFound, err.Error(), "")
	}

	switch r := mcpReq.(type) {
	case mcp.InitializeRequest:
		state, err := sessionFromHeader(req, secret)
		if err != nil {
			return rpcErrorResponse(rpcReq.Id, rpc.InvalidRequest, err.Error(), "")
		}
		token, err := encodeSession(state, secret)
		if err != nil {
			return rpcErrorResponse(rpcReq.Id, rpc.InternalError, err.Error(), "")
		}

		resp, err := rpcResponse(mcp.InitializeResponse{
			ProtocolVersion: mcp.Version,
			ServerInfo: mcp.Info{
				Name:    functionName,
				Version: "1.0.0-netlify-lite",
			},
			Capabilities: mcp.Capabilities{"tools": mcp.Capability{}},
		}, token)
		if err != nil {
			return events.APIGatewayProxyResponse{
				StatusCode: http.StatusInternalServerError,
				Headers:    responseHeaders(),
				Body:       err.Error(),
			}, nil
		}
		return resp, nil

	case mcp.NotificationsInitializedRequest:
		return emptyResponse(http.StatusAccepted), nil

	case mcp.NotificationsCancelledRequest:
		return emptyResponse(http.StatusAccepted), nil

	case mcp.PingRequest:
		state, err := sessionFromHeader(req, secret)
		if err != nil {
			return rpcErrorResponse(rpcReq.Id, rpc.InvalidRequest, err.Error(), "")
		}
		token, err := encodeSession(state, secret)
		if err != nil {
			return rpcErrorResponse(rpcReq.Id, rpc.InternalError, err.Error(), "")
		}
		resp, err := rpcResponse(struct{}{}, token)
		if err != nil {
			return events.APIGatewayProxyResponse{StatusCode: http.StatusInternalServerError, Headers: responseHeaders(), Body: err.Error()}, nil
		}
		return resp, nil

	case mcp.ResourcesListRequest:
		state, err := sessionFromHeader(req, secret)
		if err != nil {
			return rpcErrorResponse(rpcReq.Id, rpc.InvalidRequest, err.Error(), "")
		}
		token, err := encodeSession(state, secret)
		if err != nil {
			return rpcErrorResponse(rpcReq.Id, rpc.InternalError, err.Error(), "")
		}
		resp, err := rpcResponse(struct{}{}, token)
		if err != nil {
			return events.APIGatewayProxyResponse{StatusCode: http.StatusInternalServerError, Headers: responseHeaders(), Body: err.Error()}, nil
		}
		return resp, nil

	case mcp.PromptsListRequest:
		state, err := sessionFromHeader(req, secret)
		if err != nil {
			return rpcErrorResponse(rpcReq.Id, rpc.InvalidRequest, err.Error(), "")
		}
		token, err := encodeSession(state, secret)
		if err != nil {
			return rpcErrorResponse(rpcReq.Id, rpc.InternalError, err.Error(), "")
		}
		resp, err := rpcResponse(struct{}{}, token)
		if err != nil {
			return events.APIGatewayProxyResponse{StatusCode: http.StatusInternalServerError, Headers: responseHeaders(), Body: err.Error()}, nil
		}
		return resp, nil

	case mcp.ToolsListRequest:
		state, err := sessionFromHeader(req, secret)
		if err != nil {
			return rpcErrorResponse(rpcReq.Id, rpc.InvalidRequest, err.Error(), "")
		}
		token, err := encodeSession(state, secret)
		if err != nil {
			return rpcErrorResponse(rpcReq.Id, rpc.InternalError, err.Error(), "")
		}
		resp, err := rpcResponse(mcp.ToolsListResponse{Tools: tools()}, token)
		if err != nil {
			return events.APIGatewayProxyResponse{StatusCode: http.StatusInternalServerError, Headers: responseHeaders(), Body: err.Error()}, nil
		}
		return resp, nil

	case mcp.ToolsCallRequest:
		state, err := sessionFromHeader(req, secret)
		if err != nil {
			return rpcErrorResponse(rpcReq.Id, rpc.InvalidRequest, err.Error(), "")
		}

		result, nextState, err := callTool(ctx, cdpURL, state, r)
		token, tokenErr := encodeSession(nextState, secret)
		if tokenErr != nil {
			return rpcErrorResponse(rpcReq.Id, rpc.InternalError, tokenErr.Error(), "")
		}
		if err != nil {
			slog.Error("tool call failed", slog.String("tool", r.Params.Name), slog.Any("err", err))
			resp, respErr := rpcResponse(mcp.ToolsCallResponse{
				IsError: true,
				Content: []mcp.ToolsCallContent{{
					Type: "text",
					Text: err.Error(),
				}},
			}, token)
			if respErr != nil {
				return events.APIGatewayProxyResponse{StatusCode: http.StatusInternalServerError, Headers: responseHeaders(), Body: respErr.Error()}, nil
			}
			return resp, nil
		}

		resp, respErr := rpcResponse(mcp.ToolsCallResponse{
			Content: []mcp.ToolsCallContent{{
				Type: "text",
				Text: result,
			}},
		}, token)
		if respErr != nil {
			return events.APIGatewayProxyResponse{StatusCode: http.StatusInternalServerError, Headers: responseHeaders(), Body: respErr.Error()}, nil
		}
		return resp, nil
	}

	return rpcErrorResponse(rpcReq.Id, rpc.MethodNotFound, "unsupported method", "")
}

func main() {
	lambda.Start(handler)
}
