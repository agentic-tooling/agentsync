// Package openrouter is the small slice of OpenRouter's API that optimize
// needs: the public models list and the alpha Decisions endpoint.
package openrouter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultBaseURL is OpenRouter's API host. Tests point Client.BaseURL at an
// httptest server instead.
const DefaultBaseURL = "https://openrouter.ai"

// maxBody bounds what is read from any response; the full models list is
// under 1 MB.
const maxBody = 16 << 20

// Client calls OpenRouter. Key is sent only on Decide; the models list is
// public.
type Client struct {
	BaseURL string
	Key     string
	HTTP    *http.Client
}

// New returns a client for the real API with a per-request timeout.
func New(key string) *Client {
	return &Client{BaseURL: DefaultBaseURL, Key: key, HTTP: &http.Client{Timeout: 60 * time.Second}}
}

// Model is one entry of the models list. Prices are USD per token, as the
// decimal strings OpenRouter sends; "-1" means variable.
type Model struct {
	ID      string `json:"id"`
	Pricing struct {
		Prompt string `json:"prompt"`
	} `json:"pricing"`
}

// Question is one typed question. Criteria is a map of option to description
// for "choice" and an ordered list of levels for "score".
type Question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria"`
}

// DecisionRequest is the body of POST /api/alpha/decisions.
type DecisionRequest struct {
	Model     string              `json:"model"`
	State     []string            `json:"state"`
	Questions map[string]Question `json:"questions"`
}

// Answer is one question's answer. Pointers distinguish a missing field from
// a zero, which callers must refuse rather than read as an answer.
type Answer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Score         *float64           `json:"score"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// Decision is a Decisions response.
type Decision struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   struct {
		Cost *float64 `json:"cost"`
	} `json:"usage"`
}

// APIError is a non-2xx response, carrying OpenRouter's own message.
type APIError struct {
	Status   int
	Message  string
	Metadata map[string]any
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("OpenRouter HTTP %d: %s", e.Status, e.Message)
	// A guardrail refusal names why and where to change it; both are the fix.
	if r, ok := e.Metadata["ineligibility_reasons"]; ok {
		if b, err := json.Marshal(r); err == nil {
			msg += "; reasons: " + string(b)
		}
	}
	if u, ok := e.Metadata["configure_url"].(string); ok {
		msg += "; configure at " + u
	}
	return msg
}

// Models fetches the public models list. output_modalities=all is needed:
// the default list leaves out decision models such as Jev and Clef.
func (c *Client) Models(ctx context.Context) ([]Model, error) {
	var out struct {
		Data []Model `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/models?output_modalities=all", nil, &out); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 {
		return nil, fmt.Errorf("OpenRouter models list is empty")
	}
	return out.Data, nil
}

// Decide posts one already-encoded DecisionRequest. The caller encodes it so
// the bytes it estimated the cost of are the bytes sent.
func (c *Client) Decide(ctx context.Context, body []byte) (Decision, error) {
	var d Decision
	err := c.do(ctx, http.MethodPost, "/api/alpha/decisions", body, &d)
	return d, err
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.Key)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("reading OpenRouter response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return apiError(resp.StatusCode, data)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("OpenRouter sent malformed JSON: %w", err)
	}
	return nil
}

func apiError(status int, data []byte) error {
	var body struct {
		Error struct {
			Message  string         `json:"message"`
			Metadata map[string]any `json:"metadata"`
		} `json:"error"`
	}
	e := &APIError{Status: status}
	if json.Unmarshal(data, &body) == nil && body.Error.Message != "" {
		e.Message, e.Metadata = body.Error.Message, body.Error.Metadata
		return e
	}
	e.Message = strings.TrimSpace(string(data))
	if len(e.Message) > 200 {
		e.Message = e.Message[:200] + "..."
	}
	if e.Message == "" {
		e.Message = http.StatusText(status)
	}
	return e
}
