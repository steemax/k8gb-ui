package mutation

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
)

type attemptContextKey struct{}

// SingleAttemptTransport belongs in rest.Config.WrapTransport for the manager's
// dynamic client. dynamic.Interface itself exposes no per-request retry setting.
// It disables client-go Retry-After replay of writes, including on 429 and 5xx.
func SingleAttemptTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return singleAttemptTransport{base: base}
}

type singleAttemptTransport struct{ base http.RoundTripper }

func (transport singleAttemptTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	write := req.Method == http.MethodPost || req.Method == http.MethodPatch || req.Method == http.MethodPut || req.Method == http.MethodDelete
	if write {
		if attempt, ok := req.Context().Value(attemptContextKey{}).(*atomic.Bool); ok && attempt.Swap(true) {
			return nil, errors.New("mutation transport refused an automatic write retry")
		}
	}
	response, err := transport.base.RoundTrip(req)
	if write && response != nil && response.Header.Get("Retry-After") != "" {
		response = newResponseWithoutRetryAfter(response)
	}
	return response, err
}

func newResponseWithoutRetryAfter(response *http.Response) *http.Response {
	copy := new(http.Response)
	*copy = *response
	copy.Header = response.Header.Clone()
	copy.Header.Del("Retry-After")
	return copy
}

func singleAttemptContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, attemptContextKey{}, &atomic.Bool{})
}
