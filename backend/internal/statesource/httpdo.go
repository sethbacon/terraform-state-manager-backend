package statesource

import (
	"context"
	"io"
	"net/http"
)

// httpDo is the shared request core for the HTTP-speaking connectors
// (consul, k8s, httpbackend, and any future one): it builds an *http.Request for
// the given method/url/body, lets the caller stamp connector-specific headers via
// apply, and executes it on client. Factoring it here means a change common to
// every HTTP connector — a shared timeout, a retry policy, a response-size guard —
// has one home instead of three hand-maintained copies (#266). Only the auth /
// header injection differs per connector, so that is the sole parameter.
func httpDo(ctx context.Context, client *http.Client, method, rawURL string, body io.Reader, apply func(*http.Request)) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	if apply != nil {
		apply(req)
	}
	return client.Do(req)
}
