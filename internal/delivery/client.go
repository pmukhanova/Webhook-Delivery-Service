package delivery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/pmukhanova/webhook-delivery-service/internal/model"
	"github.com/pmukhanova/webhook-delivery-service/internal/targetpolicy"
)

type Outcome int

const (
	OutcomeUnknown Outcome = iota
	OutcomeDelivered
	OutcomeRetry
	OutcomeFailed
)

type Result struct {
	Outcome    Outcome
	HTTPStatus int
	Error      string
}

type Deliverer struct {
	client *http.Client
}

func NewDeliverer(client *http.Client) *Deliverer {
	return &Deliverer{client: client}
}

func NewHTTPClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = targetpolicy.NewSafeDialer(
		net.DefaultResolver,
		&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second},
	).DialContext
	return &http.Client{
		Transport:     transport,
		Timeout:       timeout,
		CheckRedirect: rejectRedirect,
	}
}

func rejectRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

func (d *Deliverer) Deliver(ctx context.Context, job model.WebhookJob) Result {
	if strings.ContainsAny(job.EventType, "\r\n") {
		return Result{Outcome: OutcomeFailed, Error: "event type cannot be represented as an HTTP header"}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, job.TargetURL, bytes.NewReader(job.Payload))
	if err != nil {
		return Result{Outcome: OutcomeFailed, Error: "invalid target URL"}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Webhook-ID", job.ID.String())
	request.Header.Set("X-Webhook-Event", job.EventType)

	response, err := d.client.Do(request)
	if err != nil {
		if errors.Is(err, targetpolicy.ErrBlockedDestination) {
			return Result{Outcome: OutcomeFailed, Error: "target destination is not allowed"}
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return Result{Outcome: OutcomeRetry, Error: "delivery timed out"}
		}
		if errors.Is(err, context.Canceled) {
			return Result{Outcome: OutcomeRetry, Error: "delivery canceled"}
		}
		return Result{Outcome: OutcomeRetry, Error: "network error during delivery"}
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))

	status := response.StatusCode
	switch {
	case status >= 200 && status < 300:
		return Result{Outcome: OutcomeDelivered, HTTPStatus: status}
	case status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500:
		return Result{Outcome: OutcomeRetry, HTTPStatus: status, Error: fmt.Sprintf("target returned HTTP %d", status)}
	default:
		return Result{Outcome: OutcomeFailed, HTTPStatus: status, Error: fmt.Sprintf("target returned HTTP %d", status)}
	}
}
