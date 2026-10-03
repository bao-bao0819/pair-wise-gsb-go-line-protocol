package lineprotocol

import (
	"errors"
	"math"
	"strconv"
)

// TimestampStrategy selects how timestamps are converted to a coarser precision.
type TimestampStrategy byte

const (
	// Exact requires the timestamp to be represented exactly in the output precision.
	Exact TimestampStrategy = iota

	// Floor rounds timestamps toward negative infinity.
	Floor

	// TruncateTowardZero rounds timestamps toward zero.
	TruncateTowardZero
)

var (
	// ErrTimestampInexact reports that an Exact conversion would discard part
	// of a timestamp.
	ErrTimestampInexact = errors.New("timestamp cannot be represented exactly")

	// ErrTimestampOverflow reports that timestamp multiplication overflows int64.
	ErrTimestampOverflow = errors.New("timestamp multiplication overflows int64")
)

// RewriteTimestampError identifies a timestamp rewrite failure on one line.
type RewriteTimestampError struct {
	// Line is the one-based index of the line containing the error.
	Line int

	// Column is the one-based byte column of the error when available.
	Column int

	// Op describes the failed operation.
	Op string

	// Err is the underlying error.
	Err error
}

func (e *RewriteTimestampError) Error() string {
	if e.Column > 0 {
		return "at line " + strconv.Itoa(e.Line) + ":" + strconv.Itoa(e.Column) + ": " + e.Op + ": " + e.Err.Error()
	}
	return "at line " + strconv.Itoa(e.Line) + ": " + e.Op + ": " + e.Err.Error()
}

func (e *RewriteTimestampError) Unwrap() error {
	return e.Err
}

// RewriteTimestamps converts every timestamp in lines from inPrecision to
// outPrecision. Lines without timestamps, comments, and blank lines are copied
// unchanged. Measurement, tag, field, separator, and line-ending bytes outside
// the timestamp token are also copied unchanged.
//
// Invalid lines cause RewriteTimestamps to return nil and an error whose chain
// contains either a DecodeError, ErrValueOutOfRange, ErrTimestampInexact, or
// ErrTimestampOverflow.
func RewriteTimestamps(lines []byte, inPrecision, outPrecision Precision, strategy TimestampStrategy) ([]byte, error) {
	if !validPrecision(inPrecision) {
		return nil, errors.New("invalid input precision")
	}
	if !validPrecision(outPrecision) {
		return nil, errors.New("invalid output precision")
	}
	if strategy != Exact && strategy != Floor && strategy != TruncateTowardZero {
		return nil, errors.New("invalid timestamp conversion strategy")
	}

	out := make([]byte, 0, len(lines))
	lineStart := 0
	lineNumber := 1
	for i := 0; i <= len(lines); i++ {
		if i < len(lines) && lines[i] != '\n' {
			continue
		}

		end := i
		if i < len(lines) {
			end++
		}
		line := lines[lineStart:end:end]
		if isBlankLine(line) {
			out = append(out, line...)
		} else {
			rewritten, err := rewriteTimestampLine(line, inPrecision, outPrecision, strategy)
			if err != nil {
				return nil, addRewriteLineNumber(err, lineNumber)
			}
			out = append(out, rewritten...)
		}

		lineStart = i + 1
		lineNumber++
	}
	return out, nil
}

func validPrecision(p Precision) bool {
	switch p {
	case Nanosecond, Microsecond, Millisecond, Second:
		return true
	}
	return false
}

func isBlankLine(line []byte) bool {
	trimmed := line
	for len(trimmed) > 0 && trimmed[0] == ' ' {
		trimmed = trimmed[1:]
	}
	if len(trimmed) == 0 || trimmed[0] == '\n' {
		return true
	}
	if len(trimmed) == 1 && trimmed[0] == '\r' {
		return true
	}
	if len(trimmed) == 2 && trimmed[0] == '\r' && trimmed[1] == '\n' {
		return true
	}
	return false
}

func rewriteTimestampLine(line []byte, inPrecision, outPrecision Precision, strategy TimestampStrategy) ([]byte, error) {
	d := NewDecoderWithBytes(line)
	if !d.Next() {
		return line, nil
	}
	if _, err := d.Measurement(); err != nil {
		return nil, decodeErrorWithLineOffset(err, d.r0)
	}
	for {
		key, _, err := d.NextTag()
		if err != nil {
			return nil, decodeErrorWithLineOffset(err, d.r0)
		}
		if key == nil {
			break
		}
	}
	for {
		key, _, err := d.NextField()
		if err != nil {
			return nil, decodeErrorWithLineOffset(err, d.r0)
		}
		if key == nil {
			break
		}
	}

	timeStart := d.r1
	timeBytes, err := d.TimeBytes()
	if err != nil {
		return nil, decodeErrorWithLineOffset(err, d.r0)
	}
	if timeBytes == nil {
		return line, nil
	}

	timeEnd := timeStart
	for timeEnd < len(line) && timeChars.get(line[timeEnd]) {
		timeEnd++
	}

	timestamp, err := parseIntBytes(timeBytes, 10, 64)
	if err != nil {
		return nil, &DecodeError{
			Line:   1,
			Column: timeStart + 1,
			Err:    maybeOutOfRange(err, "invalid timestamp syntax"),
		}
	}

	converted, err := convertTimestamp(timestamp, inPrecision, outPrecision, strategy)
	if err != nil {
		return nil, &RewriteTimestampError{
			Line:   1,
			Column: timeStart + 1,
			Op:     "rewrite timestamp",
			Err:    err,
		}
	}

	out := make([]byte, 0, len(line)-len(timeBytes)+20)
	out = append(out, line[:timeStart]...)
	out = strconv.AppendInt(out, converted, 10)
	out = append(out, line[timeEnd:]...)
	return out, nil
}

func decodeErrorWithLineOffset(err error, offset int) error {
	var decodeErr *DecodeError
	if !errors.As(err, &decodeErr) {
		return err
	}
	return &DecodeError{
		Line:   1,
		Column: decodeErr.Column + offset,
		Err:    decodeErr.Err,
	}
}

func convertTimestamp(timestamp int64, inPrecision, outPrecision Precision, strategy TimestampStrategy) (int64, error) {
	inScale := int64(inPrecision.Duration())
	outScale := int64(outPrecision.Duration())
	switch {
	case inScale == outScale:
		return timestamp, nil
	case inScale < outScale:
		factor := outScale / inScale
		quotient := timestamp / factor
		remainder := timestamp % factor
		if remainder != 0 && strategy == Exact {
			return 0, ErrTimestampInexact
		}
		if remainder != 0 && strategy == Floor && timestamp < 0 {
			quotient--
		}
		return quotient, nil
	default:
		factor := inScale / outScale
		if timestamp > 0 && timestamp > math.MaxInt64/factor || timestamp < 0 && timestamp < math.MinInt64/factor {
			return 0, ErrTimestampOverflow
		}
		return timestamp * factor, nil
	}
}

func addRewriteLineNumber(err error, line int) error {
	var decodeErr *DecodeError
	if errors.As(err, &decodeErr) {
		return &DecodeError{
			Line:   int64(line),
			Column: decodeErr.Column,
			Err:    decodeErr.Err,
		}
	}

	var rewriteErr *RewriteTimestampError
	if errors.As(err, &rewriteErr) {
		return &RewriteTimestampError{
			Line:   line,
			Column: rewriteErr.Column,
			Op:     rewriteErr.Op,
			Err:    rewriteErr.Err,
		}
	}
	return &RewriteTimestampError{
		Line: line,
		Op:   "rewrite timestamp",
		Err:  err,
	}
}
