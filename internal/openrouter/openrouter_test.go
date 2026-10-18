package openrouter

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// serve answers every request with status and body. last returns the most
// recent request; the mutex is what the race detector sees, not the socket.
func serve(t *testing.T, status int, body string) (c *Client, last func() *http.Request) {
	t.Helper()
	var (
		mu  sync.Mutex
		got *http.Request
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r.Clone(context.Background())
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	c = New("sk-test")
	c.BaseURL = srv.URL
	return c, func() *http.Request { mu.Lock(); defer mu.Unlock(); return got }
}

func TestModels_ParsesTheRealListAndSendsNoKey(t *testing.T) {
	c, last := serve(t, 200, fixture(t, "models.json"))
	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := last()
	if got.URL.String() != "/api/v1/models?output_modalities=all" || got.Method != http.MethodGet {
		t.Errorf("request = %s %s", got.Method, got.URL)
	}
	if h := got.Header.Get("Authorization"); h != "" {
		t.Errorf("the public list was sent Authorization %q", h)
	}
	var jev string
	for _, m := range models {
		if m.ID == "~typesafe/jev-latest" {
			jev = m.Pricing.Prompt
		}
	}
	if jev != "0.000000042" {
		t.Errorf("jev-latest prompt price = %q, want 0.000000042", jev)
	}
}

func TestModels_EmptyListRefuses(t *testing.T) {
	c, _ := serve(t, 200, `{"data":[]}`)
	if _, err := c.Models(context.Background()); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("err = %v, want an empty-list refusal", err)
	}
}

func TestDecide_ParsesRealResponses(t *testing.T) {
	for _, tc := range []struct {
		file, choice string
		prob, cost   float64
	}{
		{"decision-jev.json", "sonnet", 0.57, 0.000082152},
		{"decision-clef.json", "opus", 0.668, 0.00045312},
	} {
		t.Run(tc.file, func(t *testing.T) {
			c, last := serve(t, 200, fixture(t, tc.file))
			d, err := c.Decide(context.Background(), []byte(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			got := last()
			if got.Header.Get("Authorization") != "Bearer sk-test" || got.URL.Path != "/api/alpha/decisions" || got.Method != http.MethodPost {
				t.Errorf("request = %s %s auth %q", got.Method, got.URL, got.Header.Get("Authorization"))
			}
			m := d.Answers["model"]
			if m.Choice != tc.choice || m.Probabilities[tc.choice] != tc.prob {
				t.Errorf("model answer = %+v", m)
			}
			if e := d.Answers["effort"]; e.Score == nil || len(e.Probabilities) != 5 {
				t.Errorf("effort answer lost its score or probabilities: %+v", e)
			}
			if d.Usage.Cost == nil || *d.Usage.Cost != tc.cost {
				t.Errorf("usage.cost = %v, want %v", d.Usage.Cost, tc.cost)
			}
		})
	}
}

func TestDecide_ErrorsCarryOpenRouterMessage(t *testing.T) {
	guardrail := `{"error":{"message":"Provider not allowed by guardrail","code":404,"metadata":{"ineligibility_reasons":["provider anthropic blocked"],"configure_url":"https://openrouter.ai/settings/guardrails"}}}`
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   []string
	}{
		{"unknown model", 400, fixture(t, "error-unknown-model.json"), []string{"HTTP 400", "Model openai/gpt-6-luna does not exist"}},
		{"guardrail", 404, guardrail, []string{"HTTP 404", "Provider not allowed by guardrail", "provider anthropic blocked", "configure at https://openrouter.ai/settings/guardrails"}},
		{"plain 5xx", 502, "bad gateway\n", []string{"HTTP 502: bad gateway"}},
		{"empty 5xx", 503, "", []string{"HTTP 503: Service Unavailable"}},
		{"long body", 500, strings.Repeat("x", 300), []string{strings.Repeat("x", 200) + "..."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := serve(t, tc.status, tc.body)
			_, err := c.Decide(context.Background(), []byte(`{}`))
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Status != tc.status {
				t.Fatalf("err = %v, want an APIError with status %d", err, tc.status)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q is missing %q", err, w)
				}
			}
			if strings.Contains(err.Error(), "sk-test") {
				t.Errorf("error leaks the key: %v", err)
			}
		})
	}
}

func TestDecide_MalformedJSON(t *testing.T) {
	c, _ := serve(t, 200, `{"answers":`)
	if _, err := c.Decide(context.Background(), []byte(`{}`)); err == nil || !strings.Contains(err.Error(), "malformed JSON") {
		t.Errorf("err = %v, want malformed JSON", err)
	}
}

func TestDecide_TimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	c := &Client{BaseURL: srv.URL, Key: "sk-test", HTTP: &http.Client{Timeout: 50 * time.Millisecond}}
	_, err := c.Decide(context.Background(), []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), "Timeout") {
		t.Errorf("err = %v, want a client timeout", err)
	}
	if err != nil && strings.Contains(err.Error(), "sk-test") {
		t.Errorf("error leaks the key: %v", err)
	}
}

func TestNew_HasATimeout(t *testing.T) {
	if New("").HTTP.Timeout <= 0 {
		t.Error("New's client can wait forever")
	}
}

func TestDo_TransportAndRequestErrors(t *testing.T) {
	c := &Client{BaseURL: "http://127.0.0.1:0", Key: "sk-test", HTTP: &http.Client{}}
	if _, err := c.Models(context.Background()); err == nil {
		t.Error("a refused connection returned no error")
	}
	_, err := c.Decide(context.Background(), []byte(`{}`))
	if err == nil || strings.Contains(err.Error(), "sk-test") {
		t.Errorf("err = %v, want a refused connection that does not name the key", err)
	}
	c.BaseURL = "://bad"
	if _, err := c.Models(context.Background()); err == nil {
		t.Error("an unparseable URL returned no error")
	}
}

func TestDo_CapsTheBody(t *testing.T) {
	c, _ := serve(t, 200, `{"data":[{"id":"x"}],"pad":"`+strings.Repeat("a", maxBody)+`"}`)
	if _, err := c.Models(context.Background()); err == nil || !strings.Contains(err.Error(), "malformed JSON") {
		t.Errorf("err = %v, want the oversized body cut short", err)
	}
}

func TestDo_TruncatedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(w, `{"data":`)
	}))
	t.Cleanup(srv.Close)
	c := New("")
	c.BaseURL = srv.URL
	if _, err := c.Models(context.Background()); err == nil || !strings.Contains(err.Error(), "reading OpenRouter response") {
		t.Errorf("err = %v, want a read error", err)
	}
}
