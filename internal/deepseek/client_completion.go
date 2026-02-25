package deepseek

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"ds2api/internal/auth"
	"ds2api/internal/config"
)

func (c *Client) CallCompletion(ctx context.Context, a *auth.RequestAuth, payload map[string]any, powResp string, maxAttempts int) (*http.Response, error) {
	if maxAttempts <= 0 {
		maxAttempts = c.maxRetries
	}
	accountID := ""
	if a != nil {
		accountID = strings.TrimSpace(a.AccountID)
	}
	captureSession := c.capture.Start("deepseek_completion", DeepSeekCompletionURL, accountID, payload)
	attempts := 0
	refreshed := false
	for attempts < maxAttempts {
		headers := c.authHeaders(tokenFromRequestAuth(a))
		headers["x-ds-pow-response"] = powResp
		resp, err := c.streamPost(ctx, DeepSeekCompletionURL, headers, payload)
		if err != nil {
			config.Logger.Warn("[completion] request error", "phase", "completion", "attempt", attempts+1, "account", accountID, "error", err)
			attempts++
			time.Sleep(time.Second)
			continue
		}
		if resp.StatusCode == http.StatusOK {
			if captureSession != nil {
				resp.Body = captureSession.WrapBody(resp.Body, resp.StatusCode)
			}
			return resp, nil
		}
		reqID := firstHeader(resp.Header, "x-request-id", "x-oneapi-request-id")
		config.Logger.Warn("[completion] failed", "phase", "completion", "status", resp.StatusCode, "attempt", attempts+1, "account", accountID, "request_id", reqID)
		if captureSession != nil {
			resp.Body = captureSession.WrapBody(resp.Body, resp.StatusCode)
		}
		_ = resp.Body.Close()
		if canRecoverCompletionAuth(c, a, resp.StatusCode) {
			if isTokenInvalid(resp.StatusCode, 0, "") && !refreshed {
				if c.Auth.RefreshToken(ctx, a) {
					refreshed = true
					accountID = strings.TrimSpace(a.AccountID)
					attempts++
					time.Sleep(time.Second)
					continue
				}
			}
			if c.Auth.SwitchAccount(ctx, a) {
				refreshed = false
				accountID = strings.TrimSpace(a.AccountID)
				attempts++
				time.Sleep(time.Second)
				continue
			}
		}
		attempts++
		time.Sleep(time.Second)
	}
	return nil, errors.New("completion failed")
}

func canRecoverCompletionAuth(c *Client, a *auth.RequestAuth, status int) bool {
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		return false
	}
	if c == nil || c.Auth == nil || a == nil {
		return false
	}
	return a.UseConfigToken
}

func tokenFromRequestAuth(a *auth.RequestAuth) string {
	if a == nil {
		return ""
	}
	return strings.TrimSpace(a.DeepSeekToken)
}

func firstHeader(h http.Header, keys ...string) string {
	for _, key := range keys {
		if v := strings.TrimSpace(h.Get(key)); v != "" {
			return v
		}
	}
	return ""
}

func (c *Client) streamPost(ctx context.Context, url string, headers map[string]string, payload any) (*http.Response, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.stream.Do(req)
	if err != nil {
		config.Logger.Warn("[deepseek] fingerprint stream request failed, fallback to std transport", "url", url, "error", err)
		req2, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
		if reqErr != nil {
			return nil, err
		}
		for k, v := range headers {
			req2.Header.Set(k, v)
		}
		return c.fallbackS.Do(req2)
	}
	return resp, nil
}
