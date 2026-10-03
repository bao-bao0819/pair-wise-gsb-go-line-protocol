package lineprotocol

import (
	"errors"
	"strconv"
	"testing"
)

func TestRewriteTimestampsRoundingStrategies(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		rounding TimestampRounding
		want     string
		wantErr  error
	}{{
		name:     "exact negative divisible",
		input:    "m f=1i -1000",
		rounding: TimestampExact,
		want:     "m f=1i -1",
	}, {
		name:     "exact negative not divisible",
		input:    "m f=1i -1234",
		rounding: TimestampExact,
		wantErr:  ErrTimestampInexact,
	}, {
		name:     "floor negative",
		input:    "m f=1i -1234",
		rounding: TimestampFloor,
		want:     "m f=1i -2",
	}, {
		name:     "truncate negative toward zero",
		input:    "m f=1i -1234",
		rounding: TimestampTruncateTowardZero,
		want:     "m f=1i -1",
	}, {
		name:     "positive floor and truncate agree",
		input:    "m f=1i 1234",
		rounding: TimestampFloor,
		want:     "m f=1i 1",
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := RewriteTimestamps([]byte(test.input), Microsecond, Millisecond, test.rounding)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("error: got %v, want %v", err, test.wantErr)
				}
				if got != nil {
					t.Fatalf("output for failed rewrite: %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("RewriteTimestamps: %v", err)
			}
			if string(got) != test.want {
				t.Fatalf("output: got %q, want %q", got, test.want)
			}
		})
	}
}

func TestRewriteTimestampsBoundaryOverflow(t *testing.T) {
	maxMillisecond := int64(9223372036854)
	minMillisecond := int64(-9223372036854)

	tests := []struct {
		name      string
		timestamp string
		want      string
		wantErr   error
	}{{
		name:      "maximum millisecond",
		timestamp: "9223372036854",
		want:      "9223372036854000000",
	}, {
		name:      "one above maximum millisecond",
		timestamp: "9223372036855",
		wantErr:   ErrTimestampOverflow,
	}, {
		name:      "minimum millisecond",
		timestamp: "-9223372036854",
		want:      "-9223372036854000000",
	}, {
		name:      "one below minimum millisecond",
		timestamp: "-9223372036855",
		wantErr:   ErrTimestampOverflow,
	}, {
		name:      "lexical timestamp exceeds int64",
		timestamp: "9223372036854775808",
		wantErr:   ErrTimestampOutOfRange,
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := "m f=1i " + test.timestamp
			got, err := RewriteTimestamps([]byte(input), Millisecond, Nanosecond, TimestampExact)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("error: got %v, want %v", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("RewriteTimestamps: %v", err)
			}
			if want := "m f=1i " + test.want; string(got) != want {
				t.Fatalf("output: got %q, want %q", got, want)
			}
		})
	}

	if got, err := RewriteTimestamps([]byte("m f=1i "+strconv.FormatInt(maxMillisecond, 10)), Millisecond, Nanosecond, TimestampExact); err != nil {
		t.Fatalf("maximum boundary: %v", err)
	} else if want := "m f=1i " + strconv.FormatInt(maxMillisecond*1_000_000, 10); string(got) != want {
		t.Fatalf("maximum output: got %q, want %q", got, want)
	}

	if got, err := RewriteTimestamps([]byte("m f=1i "+strconv.FormatInt(minMillisecond, 10)), Millisecond, Nanosecond, TimestampExact); err != nil {
		t.Fatalf("minimum boundary: %v", err)
	} else if want := "m f=1i " + strconv.FormatInt(minMillisecond*1_000_000, 10); string(got) != want {
		t.Fatalf("minimum output: got %q, want %q", got, want)
	}

	_, err := RewriteTimestamps([]byte("m f=1i 9223372037"), Second, Millisecond, TimestampExact)
	if !errors.Is(err, ErrTimestampOutOfRange) {
		t.Fatalf("second-to-millisecond boundary error: got %v, want %v", err, ErrTimestampOutOfRange)
	}
}

func TestRewriteTimestampsPreservesUntimedLinesAndStringFields(t *testing.T) {
	input := "# comment\r\n" +
		"m\\ ,a=a\\ b s=\"1234 5678\",i=42i,f=3.0\r\n" +
		"next f=\"spaces and digits 9\" \r\n"

	got, err := RewriteTimestamps([]byte(input), Nanosecond, Millisecond, TimestampFloor)
	if err != nil {
		t.Fatalf("RewriteTimestamps: %v", err)
	}
	if string(got) != input {
		t.Fatalf("untimed document changed:\ngot:  %q\nwant: %q", got, input)
	}

	timed := "m f=\"1234 5678\" 1000\n"
	got, err = RewriteTimestamps([]byte(timed), Microsecond, Millisecond, TimestampExact)
	if err != nil {
		t.Fatalf("RewriteTimestamps: %v", err)
	}
	if want := "m f=\"1234 5678\" 1\n"; string(got) != want {
		t.Fatalf("output: got %q, want %q", got, want)
	}
}

func TestRewriteTimestampsInvalidLineIsAtomic(t *testing.T) {
	input := "good f=1i 10\nbad f\nlater f=2i 20\n"
	got, err := RewriteTimestamps([]byte(input), Second, Millisecond, TimestampExact)
	if err == nil {
		t.Fatalf("RewriteTimestamps succeeded with output %q", got)
	}
	if got != nil {
		t.Fatalf("invalid document produced output %q", got)
	}

	got, err = RewriteTimestamps([]byte("m f=\"unterminated 10"), Nanosecond, Second, TimestampFloor)
	if err == nil {
		t.Fatalf("RewriteTimestamps succeeded with output %q", got)
	}
	if got != nil {
		t.Fatalf("invalid line produced output %q", got)
	}

	got, err = RewriteTimestamps([]byte("good f=1i 1000\nbad f=1i 1001\n"), Millisecond, Second, TimestampExact)
	if !errors.Is(err, ErrTimestampInexact) {
		t.Fatalf("error: got %v, want %v", err, ErrTimestampInexact)
	}
	if got != nil {
		t.Fatalf("later conversion failure produced output %q", got)
	}
}

func TestRewriteTimestampsExactRoundTrip(t *testing.T) {
	original := []byte("m\\ ,tag=a\\ b s=\"1234 5678\",i=42i 42\r\nnext f=7i 7\r\n")

	milliseconds, err := RewriteTimestamps(original, Second, Millisecond, TimestampExact)
	if err != nil {
		t.Fatalf("seconds to milliseconds: %v", err)
	}
	roundTripped, err := RewriteTimestamps(milliseconds, Millisecond, Second, TimestampExact)
	if err != nil {
		t.Fatalf("milliseconds to seconds: %v", err)
	}
	if string(roundTripped) != string(original) {
		t.Fatalf("round trip changed bytes:\ngot:  %q\nwant: %q", roundTripped, original)
	}

	wantMilliseconds := []byte("m\\ ,tag=a\\ b s=\"1234 5678\",i=42i 42000\r\nnext f=7i 7000\r\n")
	if string(milliseconds) != string(wantMilliseconds) {
		t.Fatalf("milliseconds output:\ngot:  %q\nwant: %q", milliseconds, wantMilliseconds)
	}
}

func TestRewriteTimestampsPreservesLaterUntimedLine(t *testing.T) {
	input := []byte("m f=1i 1000\r\nn f=2i\r\n# comment\n")
	got, err := RewriteTimestamps(input, Microsecond, Millisecond, TimestampExact)
	if err != nil {
		t.Fatalf("RewriteTimestamps: %v", err)
	}
	want := []byte("m f=1i 1\r\nn f=2i\r\n# comment\n")
	if string(got) != string(want) {
		t.Fatalf("output:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestRewriteTimestampsPreservesTrailingWhitespaceAndMultipleSeparators(t *testing.T) {
	input := []byte("m f=1i  1000 \r\n")
	got, err := RewriteTimestamps(input, Microsecond, Millisecond, TimestampExact)
	if err != nil {
		t.Fatalf("RewriteTimestamps: %v", err)
	}
	want := []byte("m f=1i  1 \r\n")
	if string(got) != string(want) {
		t.Fatalf("output:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestRewriteTimestampsStringFieldWithNewline(t *testing.T) {
	input := []byte("m f=\"line 10\nline 20\" 1000\n")
	got, err := RewriteTimestamps(input, Microsecond, Millisecond, TimestampExact)
	if err != nil {
		t.Fatalf("RewriteTimestamps: %v", err)
	}
	want := []byte("m f=\"line 10\nline 20\" 1\n")
	if string(got) != string(want) {
		t.Fatalf("output:\ngot:  %q\nwant: %q", got, want)
	}
}
