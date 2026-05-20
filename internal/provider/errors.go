package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

type ErrorKind string

const (
	ErrorTransient  ErrorKind = "transient"
	ErrorRateLimit  ErrorKind = "rate_limit"
	ErrorAuth       ErrorKind = "auth"
	ErrorValidation ErrorKind = "validation"
	ErrorPermanent  ErrorKind = "permanent"
)

type ClassifiedError struct {
	Kind ErrorKind
	Err  error
}

func (e ClassifiedError) Error() string {
	if e.Err == nil {
		return string(e.Kind)
	}
	return e.Err.Error()
}

func (e ClassifiedError) Unwrap() error {
	return e.Err
}

func TransientError(err error) error {
	if err == nil {
		return nil
	}
	return ClassifiedError{Kind: ErrorTransient, Err: err}
}

func PermanentError(err error) error {
	if err == nil {
		return nil
	}
	return ClassifiedError{Kind: ErrorPermanent, Err: err}
}

func RateLimitError(err error) error {
	if err == nil {
		return nil
	}
	return ClassifiedError{Kind: ErrorRateLimit, Err: err}
}

func AuthError(err error) error {
	if err == nil {
		return nil
	}
	return ClassifiedError{Kind: ErrorAuth, Err: err}
}

func ValidationError(err error) error {
	if err == nil {
		return nil
	}
	return ClassifiedError{Kind: ErrorValidation, Err: err}
}

func IsTransientError(err error) bool {
	var classified ClassifiedError
	return errors.As(err, &classified) && (classified.Kind == ErrorTransient || classified.Kind == ErrorRateLimit)
}

func transportError(providerName string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return TransientError(fmt.Errorf("%s request: %w", providerName, err))
}

func statusError(providerName, status string, code int, detail string) error {
	message := strings.TrimSpace(providerName + " " + status)
	if detail = strings.TrimSpace(detail); detail != "" {
		message += ": " + detail
	}
	err := errors.New(message)
	switch {
	case code == http.StatusTooManyRequests:
		return RateLimitError(err)
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return AuthError(err)
	case code == http.StatusBadRequest || code == http.StatusUnprocessableEntity:
		return ValidationError(err)
	case code >= 500:
		return TransientError(err)
	default:
		return PermanentError(err)
	}
}
