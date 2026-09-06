package delivery

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pmukhanova/webhook-delivery-service/internal/model"
	"github.com/pmukhanova/webhook-delivery-service/internal/targetpolicy"
)

func TestDeliverHTTPClassification(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		wantOutcome Outcome
	}{
		{name: "200 delivered", status: http.StatusOK, wantOutcome: OutcomeDelivered},
		{name: "204 delivered", status: http.StatusNoContent, wantOutcome: OutcomeDelivered},
		{name: "500 retry", status: http.StatusInternalServerError, wantOutcome: OutcomeRetry},
		{name: "429 retry", status: http.StatusTooManyRequests, wantOutcome: OutcomeRetry},
		{name: "408 retry", status: http.StatusRequestTimeout, wantOutcome: OutcomeRetry},
		{name: "400 failed", status: http.StatusBadRequest, wantOutcome: OutcomeFailed},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("unexpected request method or content type")
				}
				if r.Header.Get("X-Webhook-ID") == "" || r.Header.Get("X-Webhook-Event") != "created" {
					t.Errorf("webhook headers are missing")
				}
				body, _ := io.ReadAll(r.Body)
				if string(body) != `{"id":123}` {
					t.Errorf("body = %s", body)
				}
				return &http.Response{
					StatusCode: test.status,
					Body:       io.NopCloser(strings.NewReader("response")),
					Header:     make(http.Header),
					Request:    r,
				}, nil
			})}

			job := deliveryTestJob()
			result := NewDeliverer(client).Deliver(context.Background(), job)
			if result.Outcome != test.wantOutcome || result.HTTPStatus != test.status {
				t.Errorf("result = %+v, want outcome %v and status %d", result, test.wantOutcome, test.status)
			}
		})
	}
}

func TestDeliverNetworkError(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection failed")
	})}
	result := NewDeliverer(client).Deliver(context.Background(), deliveryTestJob())
	if result.Outcome != OutcomeRetry || result.Error != "network error during delivery" {
		t.Fatalf("result = %+v, want retryable network error", result)
	}
}

func TestDeliverBlockedDestinationIsPermanentFailure(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, targetpolicy.ErrBlockedDestination
	})}
	result := NewDeliverer(client).Deliver(context.Background(), deliveryTestJob())
	if result.Outcome != OutcomeFailed || result.Error != "target destination is not allowed" {
		t.Fatalf("result = %+v, want permanent blocked-destination failure", result)
	}
}

func TestDeliverClosesResponseBody(t *testing.T) {
	body := &trackingBody{Reader: strings.NewReader("ok")}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       body,
			Header:     make(http.Header),
			Request:    request,
		}, nil
	})}
	NewDeliverer(client).Deliver(context.Background(), deliveryTestJob())
	if !body.closed {
		t.Fatal("response body was not closed")
	}
}

func TestDeliverTimeout(t *testing.T) {
	client := &http.Client{
		Timeout: 5 * time.Millisecond,
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			<-request.Context().Done()
			return nil, request.Context().Err()
		}),
	}
	job := deliveryTestJob()

	result := NewDeliverer(client).Deliver(context.Background(), job)
	if result.Outcome != OutcomeRetry || result.Error != "delivery timed out" {
		t.Fatalf("result = %+v, want timeout retry", result)
	}
}

func TestDeliverDoesNotFollowRedirects(t *testing.T) {
	calls := 0
	client := &http.Client{
		CheckRedirect: rejectRedirect,
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{
				StatusCode: http.StatusFound,
				Body:       io.NopCloser(strings.NewReader("redirect")),
				Header:     http.Header{"Location": []string{"http://127.0.0.1/internal"}},
				Request:    request,
			}, nil
		}),
	}
	result := NewDeliverer(client).Deliver(context.Background(), deliveryTestJob())
	if calls != 1 {
		t.Fatalf("transport calls = %d, want 1", calls)
	}
	if result.Outcome != OutcomeFailed || result.HTTPStatus != http.StatusFound {
		t.Fatalf("result = %+v, want permanent redirect failure", result)
	}
}

func TestNewHTTPClientSecuritySettings(t *testing.T) {
	client := NewHTTPClient(time.Second)
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("safe transport must not use environment proxies")
	}
	if transport.DialContext == nil || client.CheckRedirect == nil {
		t.Fatal("safe dialer and redirect policy must be configured")
	}
	if err := client.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("redirect policy error = %v, want http.ErrUseLastResponse", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type trackingBody struct {
	io.Reader
	closed bool
}

func (b *trackingBody) Close() error {
	b.closed = true
	return nil
}

func deliveryTestJob() model.WebhookJob {
	claimToken := uuid.New()
	return model.WebhookJob{
		ID: uuid.New(), TargetURL: "https://example.com/webhook", EventType: "created",
		Payload: []byte(`{"id":123}`), Status: model.StatusProcessing, MaxAttempts: 5,
		UpdatedAt:  time.Now(),
		ClaimToken: &claimToken,
	}
}
