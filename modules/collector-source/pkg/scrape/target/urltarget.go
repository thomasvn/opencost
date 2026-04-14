package target

import (
	"fmt"
	"io"
	"net/http"
	"time"
)

type UrlTarget struct {
	url    string
	client *http.Client
}

func NewUrlTarget(url string) *UrlTarget {
	return NewUrlTargetWithClient(url, nil)
}

// NewUrlTargetWithClient creates a UrlTarget with a custom HTTP client.
// If client is nil, uses a default client with 30-second timeout.
// Without an explicit timeout, requests rely on TCP-level timeouts which can take several minutes.
func NewUrlTargetWithClient(url string, client *http.Client) *UrlTarget {
	if client == nil {
		client = &http.Client{
			Timeout: 30 * time.Second,
		}
	}
	return &UrlTarget{
		url:    url,
		client: client,
	}
}

// Load fetches the target URL and returns its content as an io.Reader.
// Returns resp.Body which implements io.ReadCloser - caller must close it.
func (t *UrlTarget) Load() (io.Reader, error) {
	resp, err := t.client.Get(t.url)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch URL: %w", err)
	}

	return resp.Body, nil
}
