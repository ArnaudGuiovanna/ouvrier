package provider

import (
	"encoding/json"
	"fmt"
	"io"
)

const (
	maxProviderJSONResponseBytes = 8 * 1024 * 1024
	maxProviderStreamBytes       = 64 * 1024 * 1024
	maxProviderSSEFrameBytes     = 1 * 1024 * 1024
	maxProviderTextBytes         = 8 * 1024 * 1024
	maxProviderToolArgsBytes     = 1 * 1024 * 1024
	maxProviderToolCalls         = 128
	maxProviderToolIdentityBytes = 256
)

func decodeBoundedProviderJSON(reader io.Reader, destination any) error {
	if reader == nil {
		return fmt.Errorf("provider response body is nil")
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxProviderJSONResponseBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxProviderJSONResponseBytes {
		return fmt.Errorf("provider response exceeds %d bytes", maxProviderJSONResponseBytes)
	}
	return json.Unmarshal(data, destination)
}

func providerTextOverflow(current, additional int) error {
	if additional < 0 || current > maxProviderTextBytes-additional {
		return fmt.Errorf("provider response text exceeds %d bytes", maxProviderTextBytes)
	}
	return nil
}

func providerToolArgsOverflow(current, additional int) error {
	if additional < 0 || current > maxProviderToolArgsBytes-additional {
		return fmt.Errorf("provider tool arguments exceed %d bytes", maxProviderToolArgsBytes)
	}
	return nil
}

func validateProviderToolIdentity(id, name string) error {
	if len(id) > maxProviderToolIdentityBytes {
		return fmt.Errorf("provider tool call ID exceeds %d bytes", maxProviderToolIdentityBytes)
	}
	if len(name) > maxProviderToolIdentityBytes {
		return fmt.Errorf("provider tool name exceeds %d bytes", maxProviderToolIdentityBytes)
	}
	return nil
}
