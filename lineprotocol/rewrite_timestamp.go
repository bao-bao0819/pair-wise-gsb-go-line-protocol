package lineprotocol

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"strconv"
)

// TimestampRounding selects how a timestamp is rounded when it cannot be
// represented exactly in the output precision.
type TimestampRounding byte

const (
	// TimestampExact requires the timestamp to be divisible by the
	// conversion factor.
	TimestampExact TimestampRounding = iota

	// TimestampFloor rounds toward negative infinity.
	TimestampFloor

	// TimestampTruncateTowardZero rounds toward zero.
	TimestampTruncateTowardZero
)

var (
	// ErrTimestampOverflow reports that conversion multiplication
	// overflows int64.
	ErrTimestampOverflow = errors.New("timestamp multiplication overflows int64")

	// ErrTimestampInexact reports that an exact conversion lost
	// timestamp precision.
	ErrTimestampInexact = errors.New("timestamp cannot be represented exactly")

	// ErrTimestampOutOfRange reports that a timestamp is outside the
	// nanosecond time range accepted by line protocol.
	ErrTimestampOutOfRange = errors.New("timestamp is outside the line-protocol time range")
)

// RewriteTimestamps rewrites the explicit timestamps in line-protocol data
// from inputPrecision to outputPrecision. Lines without timestamps, comments,
// and empty lines are copied unchanged, and no timestamp is injected.
//
// Only timestamp lexical tokens are replaced. Measurement, tag, field, and
// line-ending bytes are preserved. String fields are parsed and therefore
// cannot be mistaken for timestamps.
//
// If any line is invalid or cannot be converted, RewriteTimestamps returns
// nil and an error without returning a partially rewritten document.
func RewriteTimestamps(data []byte, inputPrecision, outputPrecision Precision, rounding TimestampRounding) ([]byte, error) {
	inputScale, err := timestampPrecisionScale(inputPrecision)
	if err != nil {
		return nil, err
	}
	outputScale, err := timestampPrecisionScale(outputPrecision)
	if err != nil {
		return nil, err
	}
	if rounding != TimestampExact && rounding != TimestampFloor && rounding != TimestampTruncateTowardZero {
		return nil, fmt.Errorf("invalid timestamp rounding strategy %d", rounding)
	}

	decoder := NewDecoderWithBytes(data)
	var out []byte
	copiedTo := 0
	for decoder.Next() {
		if _, err := decoder.advanceToSection(timeSection); err != nil {
			return nil, err
		}

		timestampStart := decoder.r1
		timestampLine := decoder.line
		timestampBytes, err := decoder.TimeBytes()
		if err != nil {
			return nil, err
		}
		if timestampBytes == nil {
			if out != nil {
				out = append(out, data[copiedTo:decoder.r1]...)
			}
			copiedTo = decoder.r1
			continue
		}

		timestamp, err := parseIntBytes(timestampBytes, 10, 64)
		if err != nil {
			conversionErr := errors.New("invalid syntax")
			if numErr, ok := err.(*strconv.NumError); ok && numErr.Err == strconv.ErrRange {
				conversionErr = fmt.Errorf("invalid timestamp (%q): %w", timestampBytes, ErrTimestampOutOfRange)
			}
			return nil, &DecodeError{
				Line:   timestampLine,
				Column: timestampColumn(data, timestampStart),
				Err:    conversionErr,
			}
		}
		timestampEnd := timestampStart + len(timestampBytes)

		rewritten, err := convertTimestamp(timestamp, inputScale, outputScale, rounding)
		if err != nil {
			return nil, &DecodeError{
				Line:   timestampLine,
				Column: timestampColumn(data, timestampStart),
				Err: fmt.Errorf("cannot convert timestamp %q from %s to %s: %w",
					timestampBytes, inputPrecision, outputPrecision, err),
			}
		}
		rewrittenBytes := strconv.AppendInt(nil, rewritten, 10)
		if bytes.Equal(rewrittenBytes, timestampBytes) {
			if out != nil {
				out = append(out, data[copiedTo:decoder.r1]...)
			}
			copiedTo = decoder.r1
			continue
		}

		if out == nil {
			out = make([]byte, 0, len(data)+len(rewrittenBytes)-len(timestampBytes))
			out = append(out, data[:timestampStart]...)
		} else {
			out = append(out, data[copiedTo:timestampStart]...)
		}
		out = append(out, rewrittenBytes...)
		out = append(out, data[timestampEnd:decoder.r1]...)
		copiedTo = decoder.r1
	}
	if err := decoder.Err(); err != nil {
		return nil, err
	}

	if out == nil {
		return data, nil
	}
	out = append(out, data[copiedTo:]...)
	return out, nil
}

func timestampPrecisionScale(precision Precision) (int64, error) {
	switch precision {
	case Nanosecond:
		return 1, nil
	case Microsecond:
		return 1000, nil
	case Millisecond:
		return 1_000_000, nil
	case Second:
		return 1_000_000_000, nil
	}
	return 0, fmt.Errorf("invalid timestamp precision %d", precision)
}

func convertTimestamp(timestamp, inputScale, outputScale int64, rounding TimestampRounding) (int64, error) {
	var rewritten int64

	switch {
	case inputScale == outputScale:
		rewritten = timestamp
	case inputScale > outputScale:
		factor := inputScale / outputScale
		var ok bool
		rewritten, ok = multiplyInt64(timestamp, factor)
		if !ok {
			return 0, ErrTimestampOverflow
		}
	default:
		factor := outputScale / inputScale
		quotient, remainder := timestamp/factor, timestamp%factor
		switch rounding {
		case TimestampExact:
			if remainder != 0 {
				return 0, ErrTimestampInexact
			}
		case TimestampFloor:
			if timestamp < 0 && remainder != 0 {
				quotient--
			}
		case TimestampTruncateTowardZero:
		}
		rewritten = quotient
	}

	if _, ok := multiplyInt64(rewritten, outputScale); !ok {
		return 0, ErrTimestampOutOfRange
	}
	return rewritten, nil
}

func multiplyInt64(value, factor int64) (int64, bool) {
	if factor <= 0 {
		return 0, false
	}
	if value > math.MaxInt64/factor || value < math.MinInt64/factor {
		return 0, false
	}
	return value * factor, true
}

func timestampColumn(data []byte, offset int) int {
	lineStart := 0
	if index := bytes.LastIndexByte(data[:offset], '\n'); index >= 0 {
		lineStart = index + 1
	}
	return offset - lineStart + 1
}
