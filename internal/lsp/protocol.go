package lsp

import (
	"context"
	"encoding/json"
	"fmt"
)

// Position represents a position in a text document
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range represents a range in a text document
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Location represents a location in a text document
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// CallHierarchyItem represents an item in the call hierarchy
type CallHierarchyItem struct {
	Name           string `json:"name"`
	Kind           int    `json:"kind"`
	Tags           []int  `json:"tags,omitempty"`
	Detail         string `json:"detail,omitempty"`
	URI            string `json:"uri"`
	Range          Range  `json:"range"`
	SelectionRange Range  `json:"selectionRange"`
	Data           any    `json:"data,omitempty"`
}

// CallHierarchyIncomingCall represents an incoming call
type CallHierarchyIncomingCall struct {
	From       CallHierarchyItem `json:"from"`
	FromRanges []Range           `json:"fromRanges"`
}

// CallHierarchyOutgoingCall represents an outgoing call
type CallHierarchyOutgoingCall struct {
	To         CallHierarchyItem `json:"to"`
	FromRanges []Range           `json:"fromRanges"`
}

// Initialize initializes the LSP session
func (c *Client) Initialize(ctx context.Context) error {
	params := map[string]interface{}{
		"processId": nil,
		"rootUri":   c.rootURI,
		"capabilities": map[string]interface{}{
			"textDocument": map[string]interface{}{
				"callHierarchy": map[string]interface{}{
					"dynamicRegistration": false,
				},
			},
		},
	}

	resp, err := c.sendRequest("initialize", params)
	if err != nil {
		return fmt.Errorf("initialize failed: %w", err)
	}

	// Send initialized notification
	notification := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "initialized",
		"params":  map[string]interface{}{},
	}

	data, _ := json.Marshal(notification)
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
	c.stdin.Write([]byte(header))
	c.stdin.Write(data)

	_ = resp // Ignore initialize response for now
	return nil
}

// DidOpen notifies gopls that a document was opened
func (c *Client) DidOpen(uri, languageID, text string) error {
	params := map[string]interface{}{
		"textDocument": map[string]interface{}{
			"uri":        uri,
			"languageId": languageID,
			"version":    1,
			"text":       text,
		},
	}

	notification := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "textDocument/didOpen",
		"params":  params,
	}

	data, _ := json.Marshal(notification)
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
	c.stdin.Write([]byte(header))
	c.stdin.Write(data)

	return nil
}

// PrepareCallHierarchy prepares the call hierarchy for a given position
func (c *Client) PrepareCallHierarchy(uri string, pos Position) ([]CallHierarchyItem, error) {
	params := map[string]interface{}{
		"textDocument": map[string]interface{}{
			"uri": uri,
		},
		"position": pos,
	}

	resp, err := c.sendRequest("textDocument/prepareCallHierarchy", params)
	if err != nil {
		return nil, fmt.Errorf("prepareCallHierarchy failed: %w", err)
	}

	var items []CallHierarchyItem
	if err := json.Unmarshal(resp.Result, &items); err != nil {
		return nil, fmt.Errorf("failed to unmarshal call hierarchy items: %w", err)
	}

	return items, nil
}

// IncomingCalls finds all incoming calls to the given call hierarchy item
func (c *Client) IncomingCalls(item CallHierarchyItem) ([]CallHierarchyIncomingCall, error) {
	params := map[string]interface{}{
		"item": item,
	}

	resp, err := c.sendRequest("callHierarchy/incomingCalls", params)
	if err != nil {
		return nil, fmt.Errorf("incomingCalls failed: %w", err)
	}

	var calls []CallHierarchyIncomingCall
	if err := json.Unmarshal(resp.Result, &calls); err != nil {
		return nil, fmt.Errorf("failed to unmarshal incoming calls: %w", err)
	}

	return calls, nil
}

// OutgoingCalls finds all outgoing calls from the given call hierarchy item
func (c *Client) OutgoingCalls(item CallHierarchyItem) ([]CallHierarchyOutgoingCall, error) {
	params := map[string]interface{}{
		"item": item,
	}

	resp, err := c.sendRequest("callHierarchy/outgoingCalls", params)
	if err != nil {
		return nil, fmt.Errorf("outgoingCalls failed: %w", err)
	}

	var calls []CallHierarchyOutgoingCall
	if err := json.Unmarshal(resp.Result, &calls); err != nil {
		return nil, fmt.Errorf("failed to unmarshal outgoing calls: %w", err)
	}

	return calls, nil
}
