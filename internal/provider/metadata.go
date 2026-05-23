package provider

import (
	"strings"
	"time"
)

type promptCacheSupport string

const (
	promptCacheUnsupported promptCacheSupport = "unsupported"
	promptCacheAnthropic   promptCacheSupport = "anthropic"
)

func attachResponseMetadata(resp Response, providerName, model string, started time.Time, req Request, support promptCacheSupport) Response {
	resp.Metadata = ResponseMetadata{
		Provider:    strings.TrimSpace(providerName),
		Model:       strings.TrimSpace(model),
		Latency:     time.Since(started),
		PromptCache: promptCacheMetadata(req, support, resp.Metadata.PromptCache),
	}
	return resp
}

func promptCacheMetadata(req Request, support promptCacheSupport, observed PromptCacheMetadata) PromptCacheMetadata {
	key := strings.TrimSpace(req.CacheKey)
	if key == "" {
		return observed
	}
	metadata := observed
	metadata.Requested = true
	metadata.CacheKey = key
	switch support {
	case promptCacheAnthropic:
		metadata.Supported = true
		if metadata.Applied {
			return metadata
		}
		metadata.Reason = "cache key present but no stable prompt prefix was marked"
	default:
		metadata.Supported = false
		metadata.Applied = false
		if metadata.Reason == "" {
			metadata.Reason = "provider does not expose prompt cache controls"
		}
	}
	return metadata
}
