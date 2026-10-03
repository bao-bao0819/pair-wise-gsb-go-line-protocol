package lineprotocol

import (
	"bytes"
	"errors"
	"math"
	"strconv"
	"testing"
)

func TestRewriteTimestampsNegativeStrategies(t *testing.T) {
	const input = "m f=1 -1\n"

	converted, err := RewriteTimestamps([]byte(input), Nanosecond, Millisecond, Floor)
	if err != nil {
		t.Fatalf("Floor rewrite failed: %v", err)
	}
	if got, want := string(converted), "m f=1 -1\n"; got != want {
		t.Fatalf("Floor result = %q, want %q", got, want)
	}

	converted, err = RewriteTimestamps([]byte(input), Nanosecond, Millisecond, TruncateTowardZero)
	if err != nil {
		t.Fatalf("TruncateTowardZero rewrite failed: %v", err)
	}
	if got, want := string(converted), "m f=1 0\n"; got != want {
		t.Fatalf("TruncateTowardZero result = %q, want %q", got, want)
	}

	_, err = RewriteTimestamps([]byte(input), Nanosecond, Millisecond, Exact)
	if !errors.Is(err, ErrTimestampInexact) {
		t.Fatalf("Exact error = %v, want ErrTimestampInexact", err)
	}
}

func TestRewriteTimestampsExactDivision(t *testing.T) {
	input := []byte("m f=1 -1000000\r\n")
	got, err := RewriteTimestamps(input, Nanosecond, Millisecond, Exact)
	if err != nil {
		t.Fatalf("exact division rewrite failed: %v", err)
	}
	want := []byte("m f=1 -1\r\n")
	if !bytes.Equal(got, want) {
		t.Fatalf("result = %q, want %q", got, want)
	}
}

func TestRewriteTimestampsMultiplicationBoundaries(t *testing.T) {
	const factor = 1000
	tests := []struct {
		name      string
		timestamp int64
		ok        bool
	}{{
		name:      "max exact",
		timestamp: math.MaxInt64 / factor,
		ok:        true,
	}, {
		name:      "min exact",
		timestamp: math.MinInt64 / factor,
		ok:        true,
	}, {
		name:      "max overflow",
		timestamp: math.MaxInt64/factor + 1,
		ok:        false,
	}, {
		name:      "min overflow",
		timestamp: math.MinInt64/factor - 1,
		ok:        false,
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			line := []byte("m f=1 " + strconv.FormatInt(test.timestamp, 10) + "\n")
			got, err := RewriteTimestamps(line, Second, Millisecond, Exact)
			if test.ok {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				expected := test.timestamp * factor
				want := []byte("m f=1 " + strconv.FormatInt(expected, 10) + "\n")
				if !bytes.Equal(got, want) {
					t.Fatalf("result = %q, want %q", got, want)
				}
				return
			}
			if got != nil {
				t.Fatalf("result = %q, want nil on error", got)
			}
			if !errors.Is(err, ErrTimestampOverflow) {
				t.Fatalf("error = %v, want ErrTimestampOverflow", err)
			}
			if errors.Is(err, ErrValueOutOfRange) {
				t.Fatalf("overflow error was also reported as protocol range error: %v", err)
			}
		})
	}
}

func TestRewriteTimestampsProtocolRange(t *testing.T) {
	input := []byte("m f=1 1000000000\nm f=1 9223372036854775808\n")
	got, err := RewriteTimestamps(input, Nanosecond, Second, Exact)
	if got != nil {
		t.Fatalf("result = %q, want nil on error", got)
	}
	if !errors.Is(err, ErrValueOutOfRange) {
		t.Fatalf("error = %v, want ErrValueOutOfRange", err)
	}
	if errors.Is(err, ErrTimestampOverflow) {
		t.Fatalf("protocol range error was also reported as multiplication overflow: %v", err)
	}

	var decodeErr *DecodeError
	if !errors.As(err, &decodeErr) || decodeErr.Line != 2 {
		t.Fatalf("error = %v, want DecodeError on line 2", err)
	}
}

func TestRewriteTimestampsPreservesLinesWithoutTimestampAndStringFields(t *testing.T) {
	input := []byte("# comment 123\n" +
		"m f=\"value 123 456\"\n" +
		"m,t=a f=\"quoted \\\"123\\\" value\" 1000\n" +
		"blankish f=1 \n")

	got, err := RewriteTimestamps(input, Millisecond, Second, Exact)
	if err != nil {
		t.Fatalf("rewrite failed: %v", err)
	}
	want := []byte("# comment 123\n" +
		"m f=\"value 123 456\"\n" +
		"m,t=a f=\"quoted \\\"123\\\" value\" 1\n" +
		"blankish f=1 \n")
	if !bytes.Equal(got, want) {
		t.Fatalf("result = %q, want %q", got, want)
	}
}

func TestRewriteTimestampsWithLeadingSpaceAndAbsoluteTimestampPosition(t *testing.T) {
	input := []byte("  m f=\"value 123\" 2000000\n")
	got, err := RewriteTimestamps(input, Nanosecond, Millisecond, Exact)
	if err != nil {
		t.Fatalf("rewrite failed: %v", err)
	}
	want := []byte("  m f=\"value 123\" 2\n")
	if !bytes.Equal(got, want) {
		t.Fatalf("result = %q, want %q", got, want)
	}

	invalid := []byte("  m f=1 123abc\n")
	_, err = RewriteTimestamps(invalid, Nanosecond, Millisecond, Exact)
	var decodeErr *DecodeError
	if !errors.As(err, &decodeErr) || decodeErr.Column != 9 {
		t.Fatalf("error = %v, want DecodeError at column 9", err)
	}
}

func TestRewriteTimestampsInvalidLineReturnsNoHalfLine(t *testing.T) {
	input := []byte("good f=1 1000000\nbad f=1 123abc\n")
	got, err := RewriteTimestamps(input, Nanosecond, Millisecond, Exact)
	if got != nil {
		t.Fatalf("result = %q, want nil", got)
	}
	var decodeErr *DecodeError
	if !errors.As(err, &decodeErr) || decodeErr.Line != 2 {
		t.Fatalf("error = %v, want DecodeError on line 2", err)
	}
}

func TestRewriteTimestampsInvalidCommentReturnsNoOutput(t *testing.T) {
	input := []byte("# valid comment\n# invalid\x00comment\n")
	got, err := RewriteTimestamps(input, Nanosecond, Millisecond, Exact)
	if got != nil {
		t.Fatalf("result = %q, want nil", got)
	}
	var decodeErr *DecodeError
	if !errors.As(err, &decodeErr) || decodeErr.Line != 2 {
		t.Fatalf("error = %v, want DecodeError on line 2", err)
	}
}

func TestRewriteTimestampsFinalLineWithoutNewlineAndMicroseconds(t *testing.T) {
	input := []byte("m f=1 1000\nm f=\"123 in string\" 2000000")
	toMilliseconds, err := RewriteTimestamps(input, Microsecond, Millisecond, Exact)
	if err != nil {
		t.Fatalf("microsecond to millisecond rewrite failed: %v", err)
	}
	want := []byte("m f=1 1\nm f=\"123 in string\" 2000")
	if !bytes.Equal(toMilliseconds, want) {
		t.Fatalf("result = %q, want %q", toMilliseconds, want)
	}

	backToMicroseconds, err := RewriteTimestamps(toMilliseconds, Millisecond, Microsecond, Exact)
	if err != nil {
		t.Fatalf("millisecond to microsecond rewrite failed: %v", err)
	}
	wantRoundTrip := []byte("m f=1 1000\nm f=\"123 in string\" 2000000")
	if !bytes.Equal(backToMicroseconds, wantRoundTrip) {
		t.Fatalf("result = %q, want %q", backToMicroseconds, wantRoundTrip)
	}
}

func TestRewriteTimestampsOverflowReportsLine(t *testing.T) {
	input := []byte("m f=1 1\nm f=1 9223372036854776\n")
	got, err := RewriteTimestamps(input, Second, Millisecond, Exact)
	if got != nil {
		t.Fatalf("result = %q, want nil", got)
	}
	var rewriteErr *RewriteTimestampError
	if !errors.As(err, &rewriteErr) || rewriteErr.Line != 2 {
		t.Fatalf("error = %v, want RewriteTimestampError on line 2", err)
	}
	if !errors.Is(err, ErrTimestampOverflow) {
		t.Fatalf("error = %v, want ErrTimestampOverflow", err)
	}
}

func TestRewriteTimestampsExactRoundTripRestoresBytes(t *testing.T) {
	original := []byte("m,t=a f=1.0 12000000000\n" +
		"m\\ ,t\\ a=value f\\ s=\"123 456\" -42000000000\r\n" +
		"m f=0u")

	toSeconds, err := RewriteTimestamps(original, Nanosecond, Second, Exact)
	if err != nil {
		t.Fatalf("conversion to seconds failed: %v", err)
	}
	roundTripped, err := RewriteTimestamps(toSeconds, Second, Nanosecond, Exact)
	if err != nil {
		t.Fatalf("conversion back to nanoseconds failed: %v", err)
	}
	if !bytes.Equal(roundTripped, original) {
		t.Fatalf("round trip = %q, want %q", roundTripped, original)
	}
}

func TestConvertTimestampStrategies(t *testing.T) {
	tests := []struct {
		name     string
		value    int64
		strategy TimestampStrategy
		want     int64
		wantErr  error
	}{{
		name:     "negative floor",
		value:    -1500000,
		strategy: Floor,
		want:     -2,
	}, {
		name:     "negative truncate toward zero",
		value:    -1500000,
		strategy: TruncateTowardZero,
		want:     -1,
	}, {
		name:     "positive floor",
		value:    1500000,
		strategy: Floor,
		want:     1,
	}, {
		name:     "negative exact",
		value:    -1500000,
		strategy: Exact,
		wantErr:  ErrTimestampInexact,
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := convertTimestamp(test.value, Nanosecond, Millisecond, test.strategy)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("error = %v, want %v", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != test.want {
				t.Fatalf("converted = %d, want %d", got, test.want)
			}
		})
	}
}
