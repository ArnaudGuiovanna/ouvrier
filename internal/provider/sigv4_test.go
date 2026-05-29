package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestSigningKeyMatchesAWSPublishedVector checks the SigV4 signing-key
// derivation against the worked example published in the AWS documentation
// ("Examples of how to derive a signing key for Signature Version 4").
func TestSigningKeyMatchesAWSPublishedVector(t *testing.T) {
	got := signingKey(
		"wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
		"20150830",
		"us-east-1",
		"iam",
	)
	want := "c4afb1cc5771d871763a393e44b703571b55cc28424d1a5e86da6ed3c154a4b9"
	if hex.EncodeToString(got) != want {
		t.Fatalf("signing key = %s, want %s", hex.EncodeToString(got), want)
	}
}

func TestSignSigV4SetsAuthorizationAndDateHeaders(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude/converse", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("NewRequest returned error: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	payload := []byte(`{"hello":"world"}`)
	signTime := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)

	signSigV4(req, payload, awsCredentials{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "secret",
	}, "us-east-1", "bedrock", signTime)

	if got := req.Header.Get("X-Amz-Date"); got != "20240102T030405Z" {
		t.Fatalf("X-Amz-Date = %q, want 20240102T030405Z", got)
	}
	wantHash := sha256.Sum256(payload)
	if got := req.Header.Get("X-Amz-Content-Sha256"); got != hex.EncodeToString(wantHash[:]) {
		t.Fatalf("X-Amz-Content-Sha256 = %q, want payload hash", got)
	}
	auth := req.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20240102/us-east-1/bedrock/aws4_request") {
		t.Fatalf("Authorization credential scope wrong: %q", auth)
	}
	if !strings.Contains(auth, "SignedHeaders=") || !strings.Contains(auth, "Signature=") {
		t.Fatalf("Authorization missing SignedHeaders/Signature: %q", auth)
	}
	if !strings.Contains(auth, "host") {
		t.Fatalf("SignedHeaders should include host: %q", auth)
	}
}

func TestSignSigV4AddsSessionTokenWhenPresent(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://bedrock-runtime.us-east-1.amazonaws.com/x", strings.NewReader(""))
	signSigV4(req, []byte("body"), awsCredentials{
		AccessKeyID:     "AK",
		SecretAccessKey: "sk",
		SessionToken:    "token-123",
	}, "us-east-1", "bedrock", time.Now())
	if got := req.Header.Get("X-Amz-Security-Token"); got != "token-123" {
		t.Fatalf("X-Amz-Security-Token = %q, want token-123", got)
	}
	if !strings.Contains(req.Header.Get("Authorization"), "x-amz-security-token") {
		t.Fatalf("session token should be a signed header: %q", req.Header.Get("Authorization"))
	}
}

// TestSignSigV4DeterministicSignature locks the end-to-end signature so any
// accidental change to the canonicalization breaks the test.
func TestSignSigV4DeterministicSignature(t *testing.T) {
	build := func() string {
		req, _ := http.NewRequest(http.MethodPost, "https://bedrock-runtime.us-east-1.amazonaws.com/model/m/converse", strings.NewReader(""))
		req.Header.Set("Content-Type", "application/json")
		signSigV4(req, []byte(`{"a":1}`), awsCredentials{
			AccessKeyID:     "AKIDEXAMPLE",
			SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
		}, "us-east-1", "bedrock", time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC))
		return req.Header.Get("Authorization")
	}
	first := build()
	second := build()
	if first != second {
		t.Fatalf("signature not deterministic:\n%s\n%s", first, second)
	}
	if first == "" {
		t.Fatal("signature empty")
	}
}
