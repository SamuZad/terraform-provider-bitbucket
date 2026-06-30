package bitbucket

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"golang.org/x/oauth2"
)

const (
	maxRetries     = 12
	maxBackoffWait = 1 * time.Hour
)

// Error represents a error from the bitbucket api.
type Error struct {
	APIError struct {
		Message string `json:"message,omitempty"`
	} `json:"error,omitempty"`
	Type       string `json:"type,omitempty"`
	StatusCode int
	Endpoint   string
}

func (e Error) Error() string {
	return fmt.Sprintf("API Error: %d %s %s", e.StatusCode, e.Endpoint, e.APIError.Message)
}

const (
	// BitbucketEndpoint is the fqdn used to talk to bitbucket
	BitbucketEndpoint string = "https://api.bitbucket.org/"
)

// Client is the base internal Client to talk to bitbuckets API. This should be a username and password
// the password should be a app-password.
type Client struct {
	Username         *string
	Password         *string
	OAuthToken       *string
	OAuthTokenSource oauth2.TokenSource
	HTTPClient       *http.Client
}

// Do Will just call the bitbucket api but also add auth to it and some extra headers
func (c *Client) Do(method, endpoint string, payload *bytes.Buffer, contentType string) (*http.Response, error) {
	absoluteendpoint := BitbucketEndpoint + endpoint
	log.Printf("[DEBUG] Sending request to %s %s", method, absoluteendpoint)

	var bodyBytes []byte
	if payload != nil {
		bodyBytes = payload.Bytes()
		log.Printf("[DEBUG] With payload %s", string(bodyBytes))
	}

	var resp *http.Response
	for attempt := 0; ; attempt++ {
		var bodyreader io.Reader
		if bodyBytes != nil {
			bodyreader = bytes.NewReader(bodyBytes)
		}

		req, err := http.NewRequest(method, absoluteendpoint, bodyreader)
		if err != nil {
			return nil, err
		}

		if c.Username != nil && c.Password != nil {
			log.Printf("[DEBUG] Setting Basic Auth")
			req.SetBasicAuth(*c.Username, *c.Password)
		}

		if c.OAuthToken != nil {
			log.Printf("[DEBUG] Setting Bearer Token")
			bearer := "Bearer " + *c.OAuthToken
			req.Header.Add("Authorization", bearer)
		}

		if c.OAuthTokenSource != nil {
			token, err := c.OAuthTokenSource.Token()
			if err != nil {
				return nil, err
			}

			token.SetAuthHeader(req)
		}

		if bodyBytes != nil && contentType != "" {
			// Can cause bad request when putting default reviews if set.
			req.Header.Add("Content-Type", contentType)
		}

		req.Close = true

		var doErr error
		resp, doErr = c.HTTPClient.Do(req)
		log.Printf("[DEBUG] Resp: %v Err: %v", resp, doErr)
		if doErr != nil {
			return resp, doErr
		}
		if resp == nil {
			return nil, fmt.Errorf("no response from %s %s", method, absoluteendpoint)
		}

		if resp.StatusCode == http.StatusTooManyRequests && attempt < maxRetries {
			wait := backoffDuration(attempt)
			log.Printf("[WARN] 429 from %s %s; retrying in %s (attempt %d/%d)", method, endpoint, wait, attempt+1, maxRetries)
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			time.Sleep(wait)
			continue
		}

		break
	}

	if resp.StatusCode >= 400 || resp.StatusCode < 200 {
		apiError := Error{
			StatusCode: resp.StatusCode,
			Endpoint:   endpoint,
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}

		log.Printf("[DEBUG] Resp Body: %s", string(body))

		err = json.Unmarshal(body, &apiError)
		if err != nil {
			apiError.APIError.Message = string(body)
		}

		return resp, error(apiError)

	}
	return resp, nil
}

// backoffDuration returns the capped exponential backoff for the given retry
// attempt. Bitbucket Cloud does not send a Retry-After header on 429s, so we
// rely entirely on this schedule.
func backoffDuration(attempt int) time.Duration {
	backoff := time.Duration(1<<uint(attempt+1)) * time.Second
	if backoff > maxBackoffWait {
		backoff = maxBackoffWait
	}
	return backoff
}

// retryingTransport wraps an http.RoundTripper and retries 429 responses with
// the same exponential backoff as Client.Do. It is installed on the swagger
// SDK's HTTP client so 2.0 API calls get the same throttling behaviour as the
// custom 1.0 client.
type retryingTransport struct {
	base http.RoundTripper
}

func (t *retryingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var bodyBytes []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body.Close()
		bodyBytes = b
	}

	for attempt := 0; ; attempt++ {
		if bodyBytes != nil {
			req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}

		resp, err := t.base.RoundTrip(req)
		if err != nil {
			return resp, err
		}
		if resp.StatusCode != http.StatusTooManyRequests || attempt >= maxRetries {
			return resp, nil
		}

		wait := backoffDuration(attempt)
		log.Printf("[WARN] 429 from %s %s; retrying in %s (attempt %d/%d)", req.Method, req.URL.Path, wait, attempt+1, maxRetries)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		time.Sleep(wait)
	}
}

// Get is just a helper method to do but with a GET verb
func (c *Client) Get(endpoint string) (*http.Response, error) {
	return c.Do("GET", endpoint, nil, "application/json")
}

// Post is just a helper method to do but with a POST verb
func (c *Client) Post(endpoint string, jsonpayload *bytes.Buffer) (*http.Response, error) {
	return c.Do("POST", endpoint, jsonpayload, "application/json")
}

// PostNonJson is just a helper method to do but with a POST verb without Json Header
func (c *Client) PostNonJson(endpoint string, payload *bytes.Buffer) (*http.Response, error) {
	return c.Do("POST", endpoint, payload, "")
}

// PostWithContentType is just a helper method to do but with a POST verb and a provided content type
func (c *Client) PostWithContentType(endpoint, contentType string, payload *bytes.Buffer) (*http.Response, error) {
	return c.Do("POST", endpoint, payload, contentType)
}

// Put is just a helper method to do but with a PUT verb
func (c *Client) Put(endpoint string, jsonpayload *bytes.Buffer) (*http.Response, error) {
	return c.Do("PUT", endpoint, jsonpayload, "application/json")
}

// PutOnly is just a helper method to do but with a PUT verb and a nil body
func (c *Client) PutOnly(endpoint string) (*http.Response, error) {
	return c.Do("PUT", endpoint, nil, "application/json")
}

// Delete is just a helper to Do but with a DELETE verb
func (c *Client) Delete(endpoint string) (*http.Response, error) {
	return c.Do("DELETE", endpoint, nil, "application/json")
}
