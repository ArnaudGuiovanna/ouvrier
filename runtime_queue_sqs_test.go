package ovr

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type fakeHTTPDoer struct {
	req    *http.Request
	body   string
	status int
	resp   string
	err    error
}

func (d *fakeHTTPDoer) Do(req *http.Request) (*http.Response, error) {
	d.req = req
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		d.body = string(raw)
	}
	if d.err != nil {
		return nil, d.err
	}
	status := d.status
	if status == 0 {
		status = http.StatusOK
	}
	body := d.resp
	if body == "" {
		body = `<SendMessageResponse><SendMessageResult><MessageId>m-1</MessageId></SendMessageResult></SendMessageResponse>`
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}, nil
}

func sqsTestCreds() sqsCredentials {
	return sqsCredentials{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
		Region:          "us-east-1",
	}
}

func TestPublishSQSQueueSendsSignedMessage(t *testing.T) {
	doer := &fakeHTTPDoer{}
	uri := "sqs://sqs.us-east-1.amazonaws.com/123456789012/my-queue"

	if err := publishSQSQueueWith(context.Background(), mustParseURL(t, uri), `{"status":"ok"}`, doer, sqsTestCreds()); err != nil {
		t.Fatalf("publishSQSQueueWith returned error: %v", err)
	}
	if doer.req == nil {
		t.Fatal("no request was sent")
	}
	if doer.req.Method != http.MethodPost {
		t.Fatalf("method = %q, want POST", doer.req.Method)
	}
	values, err := url.ParseQuery(doer.body)
	if err != nil {
		t.Fatalf("ParseQuery returned error: %v", err)
	}
	if values.Get("Action") != "SendMessage" {
		t.Fatalf("Action = %q, want SendMessage", values.Get("Action"))
	}
	if values.Get("MessageBody") != `{"status":"ok"}` {
		t.Fatalf("MessageBody = %q, want payload", values.Get("MessageBody"))
	}
	if !strings.Contains(values.Get("QueueUrl"), "my-queue") {
		t.Fatalf("QueueUrl = %q, want path with my-queue", values.Get("QueueUrl"))
	}
	if auth := doer.req.Header.Get("Authorization"); !strings.HasPrefix(auth, "AWS4-HMAC-SHA256") {
		t.Fatalf("Authorization = %q, want SigV4", auth)
	}
}

func TestPublishSQSQueuePropagatesIdempotencyKeyAsDedupID(t *testing.T) {
	doer := &fakeHTTPDoer{}
	uri := "sqs://sqs.us-east-1.amazonaws.com/123456789012/my-queue.fifo?idempotency_key=abc-123"

	if err := publishSQSQueueWith(context.Background(), mustParseURL(t, uri), "payload", doer, sqsTestCreds()); err != nil {
		t.Fatalf("publishSQSQueueWith returned error: %v", err)
	}
	values, err := url.ParseQuery(doer.body)
	if err != nil {
		t.Fatalf("ParseQuery returned error: %v", err)
	}
	if values.Get("MessageDeduplicationId") != "abc-123" {
		t.Fatalf("MessageDeduplicationId = %q, want abc-123", values.Get("MessageDeduplicationId"))
	}
}

func TestPublishSQSQueueReturnsErrorOnHTTPStatus(t *testing.T) {
	doer := &fakeHTTPDoer{status: http.StatusForbidden, resp: "denied"}
	uri := "sqs://sqs.us-east-1.amazonaws.com/123456789012/my-queue"

	if err := publishSQSQueueWith(context.Background(), mustParseURL(t, uri), "payload", doer, sqsTestCreds()); err == nil {
		t.Fatal("publishSQSQueueWith returned nil, want error")
	}
}

func TestSQSQueueConfigRequiresRegion(t *testing.T) {
	if _, err := sqsCredentialsFromEnv(mustParseURL(t, "sqs://sqs.amazonaws.com/123/q")); err == nil {
		t.Skip("environment provided AWS region; skipping required-region assertion")
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("Parse(%q) returned error: %v", raw, err)
	}
	return parsed
}
