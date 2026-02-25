package deepseek

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"ds2api/internal/account"
	"ds2api/internal/auth"
	"ds2api/internal/config"
)

type roundTripResult struct {
	status int
	body   string
	header map[string]string
}

type sequenceDoer struct {
	results []roundTripResult
	index   int
	authz   []string
}

func (d *sequenceDoer) Do(req *http.Request) (*http.Response, error) {
	d.authz = append(d.authz, req.Header.Get("authorization"))
	if d.index >= len(d.results) {
		return nil, fmt.Errorf("no mock response at index %d", d.index)
	}
	r := d.results[d.index]
	d.index++
	h := make(http.Header)
	for k, v := range r.header {
		h.Set(k, v)
	}
	return &http.Response{
		StatusCode: r.status,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(r.body)),
	}, nil
}

func TestCallCompletionRefreshTokenOn401(t *testing.T) {
	t.Setenv("DS2API_CONFIG_JSON", `{"keys":["k1"],"accounts":[{"email":"acc1@example.com","password":"pwd1","token":"old-token"}]}`)
	store := config.LoadStore()
	pool := account.NewPool(store)
	loginCalls := 0
	resolver := auth.NewResolver(store, pool, func(ctx context.Context, acc config.Account) (string, error) {
		loginCalls++
		return "fresh-token", nil
	})
	req, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer k1")
	a, err := resolver.Determine(req)
	if err != nil {
		t.Fatalf("determine auth failed: %v", err)
	}
	defer resolver.Release(a)

	doer := &sequenceDoer{
		results: []roundTripResult{
			{status: http.StatusUnauthorized, body: `{"error":"unauthorized"}`},
			{status: http.StatusOK, body: `data: [DONE]`},
		},
	}
	c := NewClient(store, resolver)
	c.stream = doer
	c.maxRetries = 2

	resp, err := c.CallCompletion(context.Background(), a, map[string]any{"x": 1}, "pow", 2)
	if err != nil {
		t.Fatalf("CallCompletion should recover after refresh, got err: %v", err)
	}
	_ = resp.Body.Close()
	if loginCalls != 1 {
		t.Fatalf("expected one refresh login, got %d", loginCalls)
	}
	if len(doer.authz) != 2 {
		t.Fatalf("expected 2 upstream attempts, got %d", len(doer.authz))
	}
	if got := doer.authz[0]; got != "Bearer old-token" {
		t.Fatalf("unexpected first token: %q", got)
	}
	if got := doer.authz[1]; got != "Bearer fresh-token" {
		t.Fatalf("expected refreshed token on second attempt, got %q", got)
	}
}

func TestCallCompletionSwitchAccountOn401AfterRefreshFail(t *testing.T) {
	t.Setenv("DS2API_CONFIG_JSON", `{"keys":["k1"],"accounts":[{"email":"acc1@example.com","password":"pwd1","token":"bad-token"},{"email":"acc2@example.com","password":"pwd2","token":"good-token"}]}`)
	store := config.LoadStore()
	pool := account.NewPool(store)
	resolver := auth.NewResolver(store, pool, func(ctx context.Context, acc config.Account) (string, error) {
		return "", fmt.Errorf("refresh failed")
	})
	req, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer k1")
	a, err := resolver.Determine(req)
	if err != nil {
		t.Fatalf("determine auth failed: %v", err)
	}
	defer resolver.Release(a)

	doer := &sequenceDoer{
		results: []roundTripResult{
			{status: http.StatusUnauthorized, body: `{"error":"unauthorized"}`},
			{status: http.StatusOK, body: `data: [DONE]`},
		},
	}
	c := NewClient(store, resolver)
	c.stream = doer
	c.maxRetries = 2

	resp, err := c.CallCompletion(context.Background(), a, map[string]any{"x": 1}, "pow", 2)
	if err != nil {
		t.Fatalf("CallCompletion should recover by switching account, got err: %v", err)
	}
	_ = resp.Body.Close()
	if len(doer.authz) != 2 {
		t.Fatalf("expected 2 upstream attempts, got %d", len(doer.authz))
	}
	if got := doer.authz[0]; got != "Bearer bad-token" {
		t.Fatalf("unexpected first token: %q", got)
	}
	if got := doer.authz[1]; got != "Bearer good-token" {
		t.Fatalf("expected switched account token on second attempt, got %q", got)
	}
}

