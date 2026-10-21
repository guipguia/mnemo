package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Client is how a remote agent talks to the hub.
type Client struct {
	url, token string
	http       *http.Client
}

func NewClient(hubURL, token string) *Client {
	return &Client{url: hubURL, token: token, http: &http.Client{Timeout: time.Minute}}
}

func (c *Client) Push(ctx context.Context, b Batch) error {
	body, err := json.Marshal(b)
	if err != nil {
		return err
	}
	return c.do(ctx, "POST", "/api/v1/ingest", bytes.NewReader(body), nil)
}

func (c *Client) Resume(ctx context.Context, cluster, uid string) (map[string]string, error) {
	var cp map[string]string
	q := url.Values{"cluster": {cluster}, "uid": {uid}}
	return cp, c.do(ctx, "GET", "/api/v1/checkpoints?"+q.Encode(), nil, &cp)
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.url+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("hub %s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(msg))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
