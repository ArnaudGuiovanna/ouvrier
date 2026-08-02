package operate

import (
	"encoding/json"
	"fmt"
	"math"
)

func workerFilePage(input map[string]any, maxOffset int) (int, int, error) {
	offset, err := boundedWorkerInteger(input, "offset", 0, 0, maxOffset)
	if err != nil {
		return 0, 0, err
	}
	limit, err := boundedWorkerInteger(input, "limit", defaultWorkerFilePageLimit, 1, maxWorkerFilePageLimit)
	if err != nil {
		return 0, 0, err
	}
	return offset, limit, nil
}

func boundedWorkerInteger(input map[string]any, key string, fallback, min, max int) (int, error) {
	value, ok := input[key]
	if !ok || value == nil {
		return fallback, nil
	}
	var number int64
	switch typed := value.(type) {
	case int:
		number = int64(typed)
	case int8:
		number = int64(typed)
	case int16:
		number = int64(typed)
	case int32:
		number = int64(typed)
	case int64:
		number = typed
	case uint:
		if uint64(typed) > math.MaxInt64 {
			return 0, fmt.Errorf("%s is outside the supported integer range", key)
		}
		number = int64(typed)
	case uint8:
		number = int64(typed)
	case uint16:
		number = int64(typed)
	case uint32:
		number = int64(typed)
	case uint64:
		if typed > math.MaxInt64 {
			return 0, fmt.Errorf("%s is outside the supported integer range", key)
		}
		number = int64(typed)
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || math.Trunc(typed) != typed || typed < math.MinInt64 || typed > math.MaxInt64 {
			return 0, fmt.Errorf("%s must be an integer", key)
		}
		number = int64(typed)
	case float32:
		asFloat64 := float64(typed)
		if math.IsNaN(asFloat64) || math.IsInf(asFloat64, 0) || math.Trunc(asFloat64) != asFloat64 {
			return 0, fmt.Errorf("%s must be an integer", key)
		}
		number = int64(typed)
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return 0, fmt.Errorf("%s must be an integer", key)
		}
		number = parsed
	default:
		return 0, fmt.Errorf("%s must be an integer", key)
	}
	if number < int64(min) || number > int64(max) {
		return 0, fmt.Errorf("%s must be between %d and %d", key, min, max)
	}
	return int(number), nil
}
