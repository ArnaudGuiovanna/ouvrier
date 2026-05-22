package provider

import (
	"strings"
	"time"
)

func attachResponseMetadata(resp Response, providerName, model string, started time.Time) Response {
	resp.Metadata = ResponseMetadata{
		Provider: strings.TrimSpace(providerName),
		Model:    strings.TrimSpace(model),
		Latency:  time.Since(started),
	}
	return resp
}
