package events

import (
	"errors"
	"time"
)

type Option func(*config) error

type config struct {
	now            func() time.Time
	initialID      uint64
	retentionLimit int
}

func NewEventStream(opts ...Option) (*EventStream, error) {
	cfg := config{now: time.Now, retentionLimit: DefaultRetentionLimit}
	for _, opt := range opts {
		if opt == nil {
			return nil, errors.New("nil event stream option")
		}
		if err := opt(&cfg); err != nil {
			return nil, err
		}
	}
	return &EventStream{
		now:         cfg.now,
		nextID:      cfg.initialID,
		events:      newEventBuffer(cfg.retentionLimit),
		kindCounts:  make(map[EventKind]uint64),
		metricStats: newStreamMetricState(cfg.retentionLimit),
	}, nil
}

func WithClock(now func() time.Time) Option {
	return func(cfg *config) error {
		if now == nil {
			return errors.New("event stream clock is required")
		}
		cfg.now = now
		return nil
	}
}

func WithInitialID(id uint64) Option {
	return func(cfg *config) error {
		cfg.initialID = id
		return nil
	}
}
