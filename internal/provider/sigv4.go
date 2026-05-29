package provider

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// awsCredentials holds the static credentials used to sign AWS requests.
type awsCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// AWSCredentials is the exported view of the static credentials used to sign
// AWS requests. It lets other internal packages (such as the queue push
// terminals) reuse the hand-rolled SigV4 signer without duplicating it.
type AWSCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// SignSigV4 signs req in place using AWS Signature Version 4. It is a thin
// exported wrapper over the package-internal signer so callers outside this
// package can reuse the exact same canonicalization and signing-key
// derivation. See signSigV4 for the detailed contract.
func SignSigV4(req *http.Request, payload []byte, creds AWSCredentials, region, service string, signTime time.Time) {
	signSigV4(req, payload, awsCredentials(creds), region, service, signTime)
}

// signSigV4 signs an HTTP request in place using the AWS Signature Version 4
// algorithm. payload is the exact request body bytes. The request must already
// carry its final URL, method and any headers that should be signed (notably
// Host and Content-Type). The X-Amz-Date and Authorization headers (and
// X-Amz-Security-Token when a session token is present) are added by this call.
//
// This is a hand-rolled signer (no aws-sdk dependency). It implements the
// header-based signing flow with SHA-256 payload hashing.
func signSigV4(req *http.Request, payload []byte, creds awsCredentials, region, service string, signTime time.Time) {
	signTime = signTime.UTC()
	amzDate := signTime.Format("20060102T150405Z")
	dateStamp := signTime.Format("20060102")

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	req.Header.Set("Host", host)
	req.Header.Set("X-Amz-Date", amzDate)
	if creds.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", creds.SessionToken)
	}

	payloadHash := hexSHA256(payload)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	canonicalHeaders, signedHeaders := canonicalHeaders(req)

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURIPath(req.URL.Path),
		req.URL.RawQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	credentialScope := strings.Join([]string{dateStamp, region, service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credentialScope,
		hexSHA256([]byte(canonicalRequest)),
	}, "\n")

	signingKey := signingKey(creds.SecretAccessKey, dateStamp, region, service)
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	authorization := fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		creds.AccessKeyID, credentialScope, signedHeaders, signature,
	)
	req.Header.Set("Authorization", authorization)
}

func canonicalHeaders(req *http.Request) (canonical string, signed string) {
	names := make([]string, 0, len(req.Header)+1)
	values := make(map[string]string, len(req.Header)+1)

	add := func(name, value string) {
		lower := strings.ToLower(name)
		names = append(names, lower)
		values[lower] = strings.TrimSpace(value)
	}

	add("host", req.Host)
	if req.Host == "" {
		values["host"] = req.URL.Host
	}
	for name, vals := range req.Header {
		lower := strings.ToLower(name)
		if lower == "host" || lower == "authorization" {
			continue
		}
		add(name, strings.Join(vals, ","))
	}

	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		b.WriteString(name)
		b.WriteString(":")
		b.WriteString(values[name])
		b.WriteString("\n")
	}
	return b.String(), strings.Join(names, ";")
}

func canonicalURIPath(path string) string {
	if path == "" {
		return "/"
	}
	return path
}

func signingKey(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte("aws4_request"))
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func hexSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
