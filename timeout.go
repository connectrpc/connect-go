package connect

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"
)

const (
	maxHours        = math.MaxInt64 / int64(time.Hour) // how many hours fit into a time.Duration?
	maxTimeoutChars = 8                                // from gRPC protocol
)

var (
	errNoTimeout = errors.New("no timeout")
	timeoutUnits = []struct {
		size time.Duration
		char byte
	}{
		{time.Nanosecond, 'n'},
		{time.Microsecond, 'u'},
		{time.Millisecond, 'm'},
		{time.Second, 'S'},
		{time.Minute, 'M'},
		{time.Hour, 'H'},
	}
	timeoutUnitLookup = make(map[byte]time.Duration)
)

func init() {
	for _, pair := range timeoutUnits {
		timeoutUnitLookup[pair.char] = pair.size
	}
}

func parseTimeout(timeout string) (time.Duration, error) {
	if timeout == "" {
		return 0, errNoTimeout
	}
	unit, ok := timeoutUnitLookup[timeout[len(timeout)-1]]
	if !ok {
		return 0, fmt.Errorf("gRPC protocol error: timeout %q has invalid unit", timeout)
	}
	num, err := strconv.ParseInt(timeout[:len(timeout)-1], 10 /* base */, 64 /* bitsize */)
	if err != nil || num < 0 {
		return 0, fmt.Errorf("gRPC protocol error: invalid timeout %q", timeout)
	}
	if num > 99999999 { // timeout must be ASCII string of at most 8 digits
		return 0, fmt.Errorf("gRPC protocol error: timeout %q is too long", timeout)
	}
	if unit == time.Hour && num > maxHours {
		// Timeout is effectively unbounded, so ignore it. The grpc-go
		// implementation does the same thing.
		return 0, errNoTimeout
	}
	return time.Duration(num) * unit, nil
}

func encodeTimeout(t time.Duration) (string, error) {
	if t <= 0 {
		return "0n", nil
	}
	for _, pair := range timeoutUnits {
		if digits := strconv.FormatInt(int64(t/pair.size), 10 /* base */); len(digits) < maxTimeoutChars {
			return digits + string(pair.char), nil
		}
	}
	return "", errNoTimeout // shouldn't reach here
}
