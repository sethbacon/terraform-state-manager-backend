// retry.go adds a small bounded retry with exponential backoff for the
// SAFELY-RETRYABLE outbound pipeline HTTP calls, so a transient network blip or a
// provider 5xx does not immediately fail a user-facing operation (#264).
//
// Only IDEMPOTENT reads (GET/HEAD) are retried: the discovery/verification calls
// that list pipelines/repos/workflows. Mutating requests get a single attempt -
// a retry after a partial success could double-apply (create a second branch/PR
// in the repo-setup flow, or double-dispatch a CI run).
package pipelines

import (
	"net/http"
	"time"
)

const (
	retryAttempts  = 3
	retryBaseDelay = 300 * time.Millisecond
)

// doWithRetry issues req through httpClient. For an idempotent method (GET/HEAD)
// it retries on a transient transport error or a 5xx response up to retryAttempts
// times with exponential backoff, aborting if the request context is cancelled;
// any other method gets a single attempt (a mutating retry could double-apply).
// The final response (even a 5xx) is returned with its body open for the caller.
func doWithRetry(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return httpClient.Do(req)
	}
	var (
		resp *http.Response
		err  error
	)
	for attempt := 1; attempt <= retryAttempts; attempt++ {
		resp, err = httpClient.Do(req)
		retryable := err != nil || (resp != nil && resp.StatusCode >= 500)
		if !retryable || attempt == retryAttempts {
			return resp, err
		}
		if resp != nil {
			_ = resp.Body.Close() // drain before the next attempt
		}
		select {
		case <-time.After(retryBaseDelay * time.Duration(1<<(attempt-1))):
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	return resp, err
}
