//go:build smoke

package smoke

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type smokeCtx struct {
	client      *http.Client
	gatewayURL  string
	usageURL    string
	llmURL      string
	ledgerURL   string

	// per-scenario state
	userID      string
	resID       string
	idemKey     string
	idemKeySufx int // incremented to generate additional unique keys

	lastStatus  int
	lastBody    map[string]any

	savedEventID float64
	balBefore    float64
	actualSpent  float64
}

func newSmokeCtx() *smokeCtx {
	return &smokeCtx{
		client:     &http.Client{Timeout: 15 * time.Second},
		gatewayURL: getenv("GATEWAY_URL", "http://localhost:8080"),
		usageURL:   getenv("USAGE_URL", "http://localhost:8081"),
		llmURL:     getenv("LLMPROXY_URL", "http://localhost:8082"),
		ledgerURL:  getenv("LEDGER_URL", "http://localhost:8083"),
	}
}

// ── HTTP helpers ──────────────────────────────────────────────────────────────

func (c *smokeCtx) post(url string, body any, headers map[string]string) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return c.do(req)
}

func (c *smokeCtx) get(url string) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	return c.do(req)
}

func (c *smokeCtx) do(req *http.Request) error {
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("request to %s: %w", req.URL, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	c.lastStatus = resp.StatusCode
	c.lastBody = nil
	if err := json.Unmarshal(raw, &c.lastBody); err != nil {
		c.lastBody = map[string]any{"_raw": strings.TrimSpace(string(raw))}
	}
	return nil
}

// ── Field accessors ───────────────────────────────────────────────────────────

func (c *smokeCtx) strField(key string) string {
	if v, ok := c.lastBody[key].(string); ok {
		return v
	}
	return ""
}

func (c *smokeCtx) floatField(key string) float64 {
	if v, ok := c.lastBody[key].(float64); ok {
		return v
	}
	return 0
}

func (c *smokeCtx) nestedFloat(outer, inner string) float64 {
	if m, ok := c.lastBody[outer].(map[string]any); ok {
		if v, ok := m[inner].(float64); ok {
			return v
		}
	}
	return 0
}

func (c *smokeCtx) nestedStr(outer, inner string) string {
	if m, ok := c.lastBody[outer].(map[string]any); ok {
		if v, ok := m[inner].(string); ok {
			return v
		}
	}
	return ""
}

// ── Convenience ───────────────────────────────────────────────────────────────

func (c *smokeCtx) serviceURL(name string) string {
	switch name {
	case "gateway":
		return c.gatewayURL
	case "usage":
		return c.usageURL
	case "llmproxy":
		return c.llmURL
	case "ledger":
		return c.ledgerURL
	}
	return ""
}

func (c *smokeCtx) balance() (float64, error) {
	if err := c.get(fmt.Sprintf("%s/v1/users/%s/balance", c.usageURL, c.userID)); err != nil {
		return 0, err
	}
	return c.floatField("available_credits"), nil
}

func (c *smokeCtx) nextIdemKey() string {
	c.idemKeySufx++
	return fmt.Sprintf("%s:%d", c.idemKey, c.idemKeySufx)
}

func getenv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
