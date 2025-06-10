package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/celsorodrigues/mcp/pkg/mcptypes"

	"github.com/go-redis/redis/v8"
)

// SessionStore interface for session management
type SessionStore interface {
	Get(ctx context.Context, sessionID string) (*SessionData, error)
	Set(ctx context.Context, sessionID string, data *SessionData, ttl time.Duration) error
	Delete(ctx context.Context, sessionID string) error
	Cleanup(ctx context.Context) error
}

// SessionData holds session information
type SessionData struct {
	ID       string                 `json:"id"`
	Created  time.Time              `json:"created"`
	LastUsed time.Time              `json:"last_used"`
	UserID   string                 `json:"user_id,omitempty"`
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

// MemorySessionStore implements SessionStore using in-memory storage
type MemorySessionStore struct {
	sessions map[string]*SessionData
	mu       sync.RWMutex
}

func NewMemorySessionStore() *MemorySessionStore {
	return &MemorySessionStore{
		sessions: make(map[string]*SessionData),
	}
}

func (m *MemorySessionStore) Get(ctx context.Context, sessionID string) (*SessionData, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	session, exists := m.sessions[sessionID]
	if !exists {
		return nil, fmt.Errorf("session not found")
	}

	// Check if session is expired (default 30 minutes)
	if time.Since(session.LastUsed) > 30*time.Minute {
		delete(m.sessions, sessionID)
		return nil, fmt.Errorf("session expired")
	}

	return session, nil
}

func (m *MemorySessionStore) Set(ctx context.Context, sessionID string, data *SessionData, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.sessions[sessionID] = data
	return nil
}

func (m *MemorySessionStore) Delete(ctx context.Context, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.sessions, sessionID)
	return nil
}

func (m *MemorySessionStore) Cleanup(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	for id, session := range m.sessions {
		if now.Sub(session.LastUsed) > 30*time.Minute {
			delete(m.sessions, id)
		}
	}
	return nil
}

// RedisSessionStore implements SessionStore using Redis
type RedisSessionStore struct {
	client *redis.Client
	prefix string
}

func NewRedisSessionStore(redisURL, password string, db int) (*RedisSessionStore, error) {
	// Parse Redis URL or use individual components
	var rdb *redis.Client

	if redisURL != "" {
		opt, err := redis.ParseURL(redisURL)
		if err != nil {
			return nil, fmt.Errorf("failed to parse Redis URL: %w", err)
		}
		rdb = redis.NewClient(opt)
	} else {
		rdb = redis.NewClient(&redis.Options{
			Addr:     getEnv("REDIS_ADDR", "localhost:6379"),
			Password: password,
			DB:       db,
		})
	}

	// Test connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	return &RedisSessionStore{
		client: rdb,
		prefix: getEnv("REDIS_SESSION_PREFIX", "mcp:session:"),
	}, nil
}

func (r *RedisSessionStore) Get(ctx context.Context, sessionID string) (*SessionData, error) {
	key := r.prefix + sessionID
	val, err := r.client.Get(ctx, key).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, fmt.Errorf("session not found")
		}
		return nil, fmt.Errorf("failed to get session: %w", err)
	}

	var session SessionData
	if err := json.Unmarshal([]byte(val), &session); err != nil {
		return nil, fmt.Errorf("failed to unmarshal session: %w", err)
	}

	return &session, nil
}

func (r *RedisSessionStore) Set(ctx context.Context, sessionID string, data *SessionData, ttl time.Duration) error {
	key := r.prefix + sessionID

	jsonData, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("failed to marshal session: %w", err)
	}

	if err := r.client.Set(ctx, key, jsonData, ttl).Err(); err != nil {
		return fmt.Errorf("failed to set session: %w", err)
	}

	return nil
}

func (r *RedisSessionStore) Delete(ctx context.Context, sessionID string) error {
	key := r.prefix + sessionID
	if err := r.client.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("failed to delete session: %w", err)
	}
	return nil
}

func (r *RedisSessionStore) Cleanup(ctx context.Context) error {
	// Redis handles TTL automatically, so cleanup is not needed
	return nil
}

// Transport interface
type Transport interface {
	Start(ctx context.Context) error
	Send(message interface{}) error
	Close() error
}

// MCP Server
type MCPServer struct {
	name         string
	version      string
	capabilities mcptypes.ServerCapabilities
	tools        map[string]mcptypes.Tool
	initialized  bool
	mu           sync.RWMutex
}

func NewMCPServer(name, version string) *MCPServer {
	return &MCPServer{
		name:    name,
		version: version,
		capabilities: mcptypes.ServerCapabilities{
			Tools: &mcptypes.ToolsCapability{ListChanged: false},
		},
		tools: make(map[string]mcptypes.Tool),
	}
}

func (s *MCPServer) AddTool(tool mcptypes.Tool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools[tool.Name] = tool
}

func (s *MCPServer) HandleMessage(data []byte) (interface{}, error) {
	var req mcptypes.JSONRPCRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return s.createErrorResponse(nil, -32700, "Parse error", nil), nil
	}

	switch req.Method {
	case "initialize":
		return s.handleInitialize(req)
	case "tools/list":
		return s.handleToolsList(req)
	case "tools/call":
		return s.handleToolsCall(req)
	default:
		return s.createErrorResponse(req.ID, -32601, "Method not found", nil), nil
	}
}

func (s *MCPServer) handleInitialize(req mcptypes.JSONRPCRequest) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var initReq mcptypes.InitializeRequest
	if req.Params != nil {
		paramsBytes, _ := json.Marshal(req.Params)
		if err := json.Unmarshal(paramsBytes, &initReq); err != nil {
			return s.createErrorResponse(req.ID, -32602, "Invalid params", nil), nil
		}
	}

	s.initialized = true

	response := mcptypes.InitializeResponse{
		ProtocolVersion: "2024-11-05",
		Capabilities:    s.capabilities,
		ServerInfo: mcptypes.ServerInfo{
			Name:    s.name,
			Version: s.version,
		},
		Instructions: "Example MCP server with Redis session support",
	}

	return s.createSuccessResponse(req.ID, response), nil
}

func (s *MCPServer) handleToolsList(req mcptypes.JSONRPCRequest) (interface{}, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if !s.initialized {
		return s.createErrorResponse(req.ID, -32002, "Server not initialized", nil), nil
	}

	tools := make([]mcptypes.Tool, 0, len(s.tools))
	for _, tool := range s.tools {
		tools = append(tools, tool)
	}

	response := mcptypes.ToolsListResponse{Tools: tools}
	return s.createSuccessResponse(req.ID, response), nil
}

func (s *MCPServer) handleToolsCall(req mcptypes.JSONRPCRequest) (interface{}, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if !s.initialized {
		return s.createErrorResponse(req.ID, -32002, "Server not initialized", nil), nil
	}

	var callReq mcptypes.CallToolRequest
	if req.Params != nil {
		paramsBytes, _ := json.Marshal(req.Params)
		if err := json.Unmarshal(paramsBytes, &callReq); err != nil {
			return s.createErrorResponse(req.ID, -32602, "Invalid params", nil), nil
		}
	}

	tool, exists := s.tools[callReq.Name]
	if !exists {
		return s.createErrorResponse(req.ID, -32000, "Tool not found", nil), nil
	}

	// Execute the tool (this is a simple example)
	result := s.executeTool(tool, callReq.Arguments)

	response := mcptypes.CallToolResponse{
		Content: []mcptypes.ContentItem{
			{Type: "text", Text: result},
		},
	}

	return s.createSuccessResponse(req.ID, response), nil
}

func (s *MCPServer) executeTool(tool mcptypes.Tool, args map[string]interface{}) string {
	switch tool.Name {
	case "echo":
		if msg, ok := args["message"].(string); ok {
			return fmt.Sprintf("Echo: %s", msg)
		}
		return "Echo: (no message provided)"
	case "add":
		a, aOk := args["a"].(float64)
		b, bOk := args["b"].(float64)
		if aOk && bOk {
			return fmt.Sprintf("Result: %.2f", a+b)
		}
		return "Error: Invalid arguments for add"
	case "time":
		return fmt.Sprintf("Current time: %s", time.Now().Format(time.RFC3339))
	default:
		return "Tool executed successfully"
	}
}

func (s *MCPServer) createSuccessResponse(id interface{}, result interface{}) mcptypes.JSONRPCResponse {
	return mcptypes.JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
}

func (s *MCPServer) createErrorResponse(id interface{}, code int, message string, data interface{}) mcptypes.JSONRPCResponse {
	return mcptypes.JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &mcptypes.JSONRPCError{
			Code:    code,
			Message: message,
			Data:    data,
		},
	}
}

// STDIO Transport (unchanged)
type StdioTransport struct {
	server *MCPServer
	writer io.Writer
}

func NewStdioTransport(server *MCPServer) *StdioTransport {
	return &StdioTransport{
		server: server,
		writer: os.Stdout,
	}
}

func (t *StdioTransport) Start(ctx context.Context) error {
	scanner := bufio.NewScanner(os.Stdin)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			line := scanner.Text()
			if line == "" {
				continue
			}

			response, err := t.server.HandleMessage([]byte(line))
			if err != nil {
				log.Printf("Error handling message: %v", err)
				continue
			}

			if err := t.Send(response); err != nil {
				log.Printf("Error sending response: %v", err)
			}
		}
	}

	return scanner.Err()
}

func (t *StdioTransport) Send(message interface{}) error {
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(t.writer, "%s\n", data)
	return err
}

func (t *StdioTransport) Close() error {
	return nil
}

// Enhanced SSE Transport with Redis session support
type SSETransport struct {
	server       *MCPServer
	sessionStore SessionStore
	sessions     map[string]*SSESession
	mu           sync.RWMutex
}

type SSESession struct {
	id       string
	events   chan []byte
	messages chan []byte
	done     chan struct{}
}

func NewSSETransport(server *MCPServer, sessionStore SessionStore) *SSETransport {
	if sessionStore == nil {
		sessionStore = NewMemorySessionStore()
	}

	return &SSETransport{
		server:       server,
		sessionStore: sessionStore,
		sessions:     make(map[string]*SSESession),
	}
}

func (t *SSETransport) Start(ctx context.Context) error {
	// Start session cleanup goroutine
	go t.startSessionCleanup(ctx)

	mux := http.NewServeMux()

	// SSE endpoint
	mux.HandleFunc("/sse", t.handleSSE)
	// Messages endpoint
	mux.HandleFunc("/messages/", t.handleMessages)

	port := getEnv("MCP_SSE_PORT", "8080")

	server := &http.Server{
		Addr:    ":" + port,
		Handler: t.addCORSHeaders(mux),
	}

	log.Printf("SSE server starting on port %s", port)
	return server.ListenAndServe()
}

func (t *SSETransport) startSessionCleanup(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := t.sessionStore.Cleanup(ctx); err != nil {
				log.Printf("Session cleanup error: %v", err)
			}
		}
	}
}

func (t *SSETransport) handleSSE(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sessionID := generateSessionID()
	session := &SSESession{
		id:       sessionID,
		events:   make(chan []byte, 100),
		messages: make(chan []byte, 100),
		done:     make(chan struct{}),
	}

	// Store session in both memory and persistent store
	t.mu.Lock()
	t.sessions[sessionID] = session
	t.mu.Unlock()

	sessionData := &SessionData{
		ID:       sessionID,
		Created:  time.Now(),
		LastUsed: time.Now(),
		Metadata: map[string]interface{}{
			"transport":  "sse",
			"user_agent": r.UserAgent(),
		},
	}

	ctx := r.Context()
	if err := t.sessionStore.Set(ctx, sessionID, sessionData, 30*time.Minute); err != nil {
		log.Printf("Failed to store session: %v", err)
	}

	defer func() {
		t.mu.Lock()
		delete(t.sessions, sessionID)
		t.mu.Unlock()

		// Clean up session from store
		if err := t.sessionStore.Delete(ctx, sessionID); err != nil {
			log.Printf("Failed to delete session: %v", err)
		}
		close(session.done)
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Send endpoint event
	endpointEvent := fmt.Sprintf("data: {\"type\":\"endpoint\",\"uri\":\"/messages/%s\"}\n\n", sessionID)
	w.Write([]byte(endpointEvent))

	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// Keep connection alive and send events
	for {
		select {
		case event := <-session.events:
			w.Write(event)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		case <-session.done:
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (t *SSETransport) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract session ID from path
	path := strings.TrimPrefix(r.URL.Path, "/messages/")
	sessionID := path

	// Check if session exists in persistent store
	ctx := r.Context()
	sessionData, err := t.sessionStore.Get(ctx, sessionID)
	if err != nil {
		http.Error(w, "Session not found or expired", http.StatusNotFound)
		return
	}

	// Update last used time
	sessionData.LastUsed = time.Now()
	if err := t.sessionStore.Set(ctx, sessionID, sessionData, 30*time.Minute); err != nil {
		log.Printf("Failed to update session: %v", err)
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Error reading request body", http.StatusBadRequest)
		return
	}

	response, err := t.server.HandleMessage(body)
	if err != nil {
		http.Error(w, "Error processing message", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (t *SSETransport) Send(message interface{}) error {
	// This would be used to send server-initiated messages
	return nil
}

func (t *SSETransport) Close() error {
	return nil
}

func (t *SSETransport) addCORSHeaders(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		handler.ServeHTTP(w, r)
	})
}

// Enhanced Streamable HTTP Transport with Redis session support
type StreamableHTTPTransport struct {
	server       *MCPServer
	sessionStore SessionStore
}

func NewStreamableHTTPTransport(server *MCPServer, sessionStore SessionStore) *StreamableHTTPTransport {
	if sessionStore == nil {
		sessionStore = NewMemorySessionStore()
	}

	return &StreamableHTTPTransport{
		server:       server,
		sessionStore: sessionStore,
	}
}

func (t *StreamableHTTPTransport) Start(ctx context.Context) error {
	// Start session cleanup goroutine
	go t.startSessionCleanup(ctx)

	mux := http.NewServeMux()

	// health port for s-http
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	// Single MCP endpoint
	mux.HandleFunc("/mcp", t.handleMCP)
	mux.HandleFunc("/", t.handleMCP) // Also handle root

	port := getEnv("MCP_HTTP_PORT", "8081")

	server := &http.Server{
		Addr:    ":" + port,
		Handler: t.addCORSHeaders(mux),
	}

	log.Printf("Streamable HTTP server starting on port %s", port)
	log.Printf("Streamable HTTP server Health on path /health and port %s", port)
	return server.ListenAndServe()
}

func (t *StreamableHTTPTransport) startSessionCleanup(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := t.sessionStore.Cleanup(ctx); err != nil {
				log.Printf("Session cleanup error: %v", err)
			}
		}
	}
}

func (t *StreamableHTTPTransport) handleMCP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "POST":
		t.handlePOST(w, r)
	case "GET":
		t.handleGET(w, r)
	case "OPTIONS":
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (t *StreamableHTTPTransport) handlePOST(w http.ResponseWriter, r *http.Request) {
	// Check if client accepts streaming
	accept := r.Header.Get("Accept")
	supportsSSE := strings.Contains(accept, "text/event-stream")

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Error reading request body", http.StatusBadRequest)
		return
	}

	response, err := t.server.HandleMessage(body)
	if err != nil {
		http.Error(w, "Error processing message", http.StatusInternalServerError)
		return
	}

	// Create or update session
	sessionID := t.getOrCreateSession(r)
	w.Header().Set("Mcp-Session-Id", sessionID)

	if supportsSSE {
		// Send as SSE stream
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		data, _ := json.Marshal(response)
		fmt.Fprintf(w, "data: %s\n\n", data)

		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	} else {
		// Send as regular JSON response
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}
}

func (t *StreamableHTTPTransport) handleGET(w http.ResponseWriter, r *http.Request) {
	// Check if client wants SSE
	accept := r.Header.Get("Accept")
	if !strings.Contains(accept, "text/event-stream") {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sessionID := t.getOrCreateSession(r)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Mcp-Session-Id", sessionID)

	// Send initial message or keep alive
	fmt.Fprintf(w, "data: {\"type\":\"connection\",\"sessionId\":\"%s\"}\n\n", sessionID)

	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// Keep connection alive
	select {
	case <-r.Context().Done():
		return
	case <-time.After(30 * time.Second):
		return
	}
}

func (t *StreamableHTTPTransport) getOrCreateSession(r *http.Request) string {
	sessionID := r.Header.Get("Mcp-Session-Id")
	ctx := r.Context()

	if sessionID != "" {
		// Try to get existing session
		sessionData, err := t.sessionStore.Get(ctx, sessionID)
		if err == nil {
			// Update last used time
			sessionData.LastUsed = time.Now()
			if err := t.sessionStore.Set(ctx, sessionID, sessionData, 30*time.Minute); err != nil {
				log.Printf("Failed to update session: %v", err)
			}
			return sessionID
		}
	}

	// Create new session
	sessionID = generateSessionID()
	sessionData := &SessionData{
		ID:       sessionID,
		Created:  time.Now(),
		LastUsed: time.Now(),
		Metadata: map[string]interface{}{
			"transport":  "http",
			"user_agent": r.UserAgent(),
		},
	}

	if err := t.sessionStore.Set(ctx, sessionID, sessionData, 30*time.Minute); err != nil {
		log.Printf("Failed to create session: %v", err)
	}

	return sessionID
}

func (t *StreamableHTTPTransport) Send(message interface{}) error {
	// This would be used for server-initiated messages in a streaming context
	return nil
}

func (t *StreamableHTTPTransport) Close() error {
	return nil
}

func (t *StreamableHTTPTransport) addCORSHeaders(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Accept, Mcp-Session-Id")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		handler.ServeHTTP(w, r)
	})
}

// Utility functions
func generateSessionID() string {
	return fmt.Sprintf("session_%d", time.Now().UnixNano())
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func createSessionStore() SessionStore {
	// Check if Redis is configured
	redisURL := os.Getenv("REDIS_URL")
	redisPassword := os.Getenv("REDIS_PASSWORD")

	if redisURL != "" || os.Getenv("REDIS_ADDR") != "" {
		log.Println("Configuring Redis session store...")
		redisStore, err := NewRedisSessionStore(redisURL, redisPassword, 0)
		if err != nil {
			log.Printf("Failed to initialize Redis session store: %v", err)
			log.Println("Falling back to memory session store")
			return NewMemorySessionStore()
		}
		log.Println("Redis session store configured successfully")
		return redisStore
	}

	log.Println("Using memory session store")
	return NewMemorySessionStore()
}

func main() {
	// Create MCP server
	server := NewMCPServer("example-mcp-server", "1.0.0")

	// Add some example tools
	server.AddTool(mcptypes.Tool{
		Name:        "echo",
		Description: "Echo back a message",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"message": map[string]interface{}{
					"type":        "string",
					"description": "The message to echo back",
				},
			},
			"required": []string{"message"},
		},
	})

	server.AddTool(mcptypes.Tool{
		Name:        "add",
		Description: "Add two numbers",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"a": map[string]interface{}{
					"type":        "number",
					"description": "First number",
				},
				"b": map[string]interface{}{
					"type":        "number",
					"description": "Second number",
				},
			},
			"required": []string{"a", "b"},
		},
	})

	server.AddTool(mcptypes.Tool{
		Name:        "time",
		Description: "Get the current time",
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	})

	// Create session store (Redis or Memory)
	sessionStore := createSessionStore()

	// Determine transport mode
	transportMode := getEnv("MCP_TRANSPORT", "http")

	ctx := context.Background()

	switch transportMode {
	case "stdio":
		log.Println("Starting MCP server with STDIO transport")
		transport := NewStdioTransport(server)
		if err := transport.Start(ctx); err != nil {
			log.Fatalf("STDIO transport error: %v", err)
		}

	case "sse":
		log.Println("Starting MCP server with SSE transport")
		transport := NewSSETransport(server, sessionStore)
		if err := transport.Start(ctx); err != nil {
			log.Fatalf("SSE transport error: %v", err)
		}

	case "http", "streamable":
		log.Println("Starting MCP server with Streamable HTTP transport")
		transport := NewStreamableHTTPTransport(server, sessionStore)
		if err := transport.Start(ctx); err != nil {
			log.Fatalf("Streamable HTTP transport error: %v", err)
		}

	default:
		log.Fatalf("Unknown transport mode: %s. Use 'stdio', 'sse', or 'http'", transportMode)
	}
}
