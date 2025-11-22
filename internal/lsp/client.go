package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
)

// Client represents an LSP client that communicates with gopls
type Client struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	nextID  atomic.Int64
	pending map[int64]chan *Response
	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	rootURI string
}

// Request represents an LSP request
type Request struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int64       `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

// Response represents an LSP response
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *ResponseError  `json:"error,omitempty"`
}

// ResponseError represents an LSP error
type ResponseError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// NewClient creates a new LSP client and starts gopls
func NewClient(ctx context.Context, rootPath string) (*Client, error) {
	ctx, cancel := context.WithCancel(ctx)

	cmd := exec.CommandContext(ctx, "gopls", "serve")

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("failed to start gopls: %w", err)
	}

	client := &Client{
		cmd:     cmd,
		stdin:   stdin,
		stdout:  stdout,
		stderr:  stderr,
		pending: make(map[int64]chan *Response),
		ctx:     ctx,
		cancel:  cancel,
		rootURI: "file://" + rootPath,
	}

	// Start reading responses
	go client.readResponses()

	return client, nil
}

// Close closes the LSP client
func (c *Client) Close() error {
	c.cancel()
	c.stdin.Close()
	return c.cmd.Wait()
}

// sendRequest sends a request and returns the response
func (c *Client) sendRequest(method string, params interface{}) (*Response, error) {
	id := c.nextID.Add(1)

	req := Request{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	// Create response channel
	respChan := make(chan *Response, 1)
	c.mu.Lock()
	c.pending[id] = respChan
	c.mu.Unlock()

	// Send request
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
	if _, err := c.stdin.Write([]byte(header)); err != nil {
		return nil, fmt.Errorf("failed to write header: %w", err)
	}
	if _, err := c.stdin.Write(data); err != nil {
		return nil, fmt.Errorf("failed to write body: %w", err)
	}

	// Wait for response
	select {
	case resp := <-respChan:
		if resp.Error != nil {
			return nil, fmt.Errorf("LSP error: %s", resp.Error.Message)
		}
		return resp, nil
	case <-c.ctx.Done():
		return nil, c.ctx.Err()
	}
}

// readResponses reads responses from gopls
func (c *Client) readResponses() {
	reader := bufio.NewReader(c.stdout)

	for {
		// Read headers
		var contentLength int
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				if err != io.EOF {
					fmt.Printf("Error reading header: %v\n", err)
				}
				return
			}

			line = strings.TrimSpace(line)

			// Empty line marks end of headers
			if line == "" {
				break
			}

			// Parse Content-Length header
			if strings.HasPrefix(line, "Content-Length:") {
				fmt.Sscanf(line, "Content-Length: %d", &contentLength)
			}
		}

		if contentLength == 0 {
			continue
		}

		// Read body
		body := make([]byte, contentLength)
		if _, err := io.ReadFull(reader, body); err != nil {
			fmt.Printf("Error reading response body: %v\n", err)
			continue
		}

		// Parse response or notification
		var msg map[string]interface{}
		if err := json.Unmarshal(body, &msg); err != nil {
			fmt.Printf("Error unmarshaling message: %v\n", err)
			continue
		}

		// Check if it's a response (has ID) or notification (no ID)
		if _, ok := msg["id"]; ok {
			var resp Response
			if err := json.Unmarshal(body, &resp); err != nil {
				fmt.Printf("Error unmarshaling response: %v\n", err)
				continue
			}

			// Send to pending channel
			c.mu.Lock()
			if ch, ok := c.pending[resp.ID]; ok {
				ch <- &resp
				delete(c.pending, resp.ID)
			}
			c.mu.Unlock()
		}
		// Ignore notifications for now
	}
}
