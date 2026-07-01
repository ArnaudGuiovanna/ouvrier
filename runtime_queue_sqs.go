package ovr

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

const sqsService = "sqs"

// queueHTTPDoer is the minimal HTTP boundary used by the SQS push terminal so
// the SigV4-signed SendMessage call can be exercised against a fake doer in
// unit tests without touching the network.
type queueHTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type sqsCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Region          string
}

// publishSQSQueue sends the outcome to an Amazon SQS queue using the
// SendMessage API. SQS SendMessage is a signed HTTPS request: the call is
// SigV4-signed (reusing the hand-rolled signer from the Bedrock adapter, no
// aws-sdk) and dispatched through the default HTTP client.
//
// The sqs:// URI encodes the endpoint host and the queue path, e.g.
// sqs://sqs.us-east-1.amazonaws.com/123456789012/my-queue. Credentials and the
// region are read from the standard AWS_* environment variables (the region may
// also be inferred from the endpoint host).
func publishSQSQueue(ctx context.Context, uri *url.URL, output string) error {
	creds, err := sqsCredentialsFromEnv(uri)
	if err != nil {
		return err
	}
	ctx, cancel := egressContext(ctx)
	defer cancel()
	return publishSQSQueueWith(ctx, uri, output, egressHTTPClient, creds)
}

func publishSQSQueueWith(ctx context.Context, uri *url.URL, output string, doer queueHTTPDoer, creds sqsCredentials) error {
	if ctx == nil {
		ctx = context.Background()
	}
	queueURL, err := sqsQueueURL(uri)
	if err != nil {
		return err
	}

	form := url.Values{}
	form.Set("Action", "SendMessage")
	form.Set("Version", "2012-11-05")
	form.Set("QueueUrl", queueURL)
	form.Set("MessageBody", output)
	if key := queueIdempotencyKey(uri); key != "" {
		// MessageDeduplicationId is honoured by FIFO queues; SQS ignores it for
		// standard queues, so it is safe to always propagate.
		form.Set("MessageDeduplicationId", key)
	}
	payload := []byte(form.Encode())

	endpoint := &url.URL{Scheme: "https", Host: uri.Host, Path: "/"}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	provider.SignSigV4(req, payload, provider.AWSCredentials{
		AccessKeyID:     creds.AccessKeyID,
		SecretAccessKey: creds.SecretAccessKey,
		SessionToken:    creds.SessionToken,
	}, creds.Region, sqsService, time.Now())

	resp, err := doer.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return fmt.Errorf("sqs queue push returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	return nil
}

// sqsQueueURL reconstructs the canonical HTTPS queue URL that SQS expects in the
// QueueUrl parameter from the sqs:// URI.
func sqsQueueURL(uri *url.URL) (string, error) {
	if uri == nil || strings.TrimSpace(uri.Host) == "" {
		return "", fmt.Errorf("sqs queue host is required")
	}
	path := strings.Trim(strings.TrimSpace(uri.Path), "/")
	if path == "" {
		return "", fmt.Errorf("sqs queue path is required")
	}
	return (&url.URL{Scheme: "https", Host: uri.Host, Path: "/" + path}).String(), nil
}

func sqsCredentialsFromEnv(uri *url.URL) (sqsCredentials, error) {
	creds := sqsCredentials{
		AccessKeyID:     strings.TrimSpace(os.Getenv("AWS_ACCESS_KEY_ID")),
		SecretAccessKey: strings.TrimSpace(os.Getenv("AWS_SECRET_ACCESS_KEY")),
		SessionToken:    strings.TrimSpace(os.Getenv("AWS_SESSION_TOKEN")),
		Region:          strings.TrimSpace(os.Getenv("AWS_REGION")),
	}
	if creds.Region == "" {
		creds.Region = strings.TrimSpace(os.Getenv("AWS_DEFAULT_REGION"))
	}
	if creds.Region == "" {
		creds.Region = sqsRegionFromHost(uri)
	}
	if creds.AccessKeyID == "" {
		return sqsCredentials{}, fmt.Errorf("sqs queue push requires AWS_ACCESS_KEY_ID")
	}
	if creds.SecretAccessKey == "" {
		return sqsCredentials{}, fmt.Errorf("sqs queue push requires AWS_SECRET_ACCESS_KEY")
	}
	if creds.Region == "" {
		return sqsCredentials{}, fmt.Errorf("sqs queue push requires AWS_REGION")
	}
	return creds, nil
}

// sqsRegionFromHost extracts the region from a standard SQS endpoint host such
// as sqs.us-east-1.amazonaws.com.
func sqsRegionFromHost(uri *url.URL) string {
	if uri == nil {
		return ""
	}
	host := uri.Hostname()
	parts := strings.Split(host, ".")
	if len(parts) >= 3 && parts[0] == "sqs" {
		return parts[1]
	}
	return ""
}
