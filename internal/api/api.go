package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/paidwen/paidwen-cli/internal/contract"
)

const maxAnswer = 4 << 20

type Provider interface {
	Authorization(ctx context.Context) (string, error)
	Invalidate() bool
	Identity() string
	Source() string
}

type Client struct {
	BaseURL     string
	Credentials Provider
	HTTP        *http.Client
	UserAgent   string
}

type Error struct {
	Status     int
	Problem    contract.Problem
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	if e.Problem.Detail != nil && *e.Problem.Detail != "" {
		return *e.Problem.Detail
	}
	if e.Problem.Title != "" {
		return e.Problem.Title
	}
	return "HTTP " + strconv.Itoa(e.Status)
}

type TokenError struct {
	Status      int
	Code        contract.TokenErrorError
	Description string
}

func (e *TokenError) Error() string {
	if e.Description != "" {
		return string(e.Code) + ": " + e.Description
	}
	return string(e.Code)
}

type ConfigError struct {
	Err error
}

func (e *ConfigError) Error() string {
	return e.Err.Error()
}

func (e *ConfigError) Unwrap() error {
	return e.Err
}

func Unavailable(err error) bool {
	if err == nil {
		return false
	}
	var config *ConfigError
	if errors.As(err, &config) {
		return false
	}
	var problem *Error
	if errors.As(err, &problem) {
		return problem.Status >= 500
	}
	var token *TokenError
	if errors.As(err, &token) {
		return token.Status >= 500
	}
	return true
}

func (c *Client) Send(ctx context.Context, method, path string, header http.Header, body any, out any) (http.Header, error) {
	var payload []byte
	switch typed := body.(type) {
	case nil:
	case []byte:
		payload = typed
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return nil, &ConfigError{Err: err}
		}
		payload = encoded
	}
	for attempt := 0; ; attempt++ {
		request, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bytes.NewReader(payload))
		if err != nil {
			return nil, &ConfigError{Err: err}
		}
		for name, values := range header {
			request.Header[name] = values
		}
		if payload != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		request.Header.Set("Accept", "application/json")
		request.Header.Set("User-Agent", c.UserAgent)
		if c.Credentials != nil {
			authorization, err := c.Credentials.Authorization(ctx)
			if err != nil {
				return nil, err
			}
			request.Header.Set("Authorization", authorization)
		}
		response, err := c.HTTP.Do(request)
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, maxAnswer))
		_ = response.Body.Close()
		if err != nil {
			return nil, err
		}
		if response.StatusCode == http.StatusUnauthorized && attempt == 0 && c.Credentials != nil && c.Credentials.Invalidate() {
			continue
		}
		if response.StatusCode >= 300 {
			return response.Header, problemOf(response, data)
		}
		if out != nil && len(data) > 0 {
			if err := json.Unmarshal(data, out); err != nil {
				return response.Header, fmt.Errorf("the answer of Paidwen is not the JSON the contract describes: %w", err)
			}
		}
		return response.Header, nil
	}
}

func problemOf(response *http.Response, data []byte) *Error {
	failure := &Error{Status: response.StatusCode, RetryAfter: retryAfter(response.Header)}
	if json.Unmarshal(data, &failure.Problem) != nil {
		failure.Problem = contract.Problem{}
	}
	return failure
}

func retryAfter(header http.Header) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(header.Get("Retry-After")))
	if err != nil || seconds < 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func PostForm(ctx context.Context, client *http.Client, userAgent, address string, form url.Values, out any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, address, strings.NewReader(form.Encode()))
	if err != nil {
		return &ConfigError{Err: err}
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", userAgent)
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxAnswer))
	_ = response.Body.Close()
	if err != nil {
		return err
	}
	if response.StatusCode >= 300 {
		var answer contract.TokenError
		failure := &TokenError{Status: response.StatusCode}
		if json.Unmarshal(data, &answer) == nil {
			failure.Code = answer.Error
			if answer.ErrorDescription != nil {
				failure.Description = *answer.ErrorDescription
			}
		}
		if failure.Code == "" {
			failure.Code = contract.TokenErrorErrorServerError
		}
		return failure
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("the answer of Paidwen is not the JSON the contract describes: %w", err)
	}
	return nil
}
