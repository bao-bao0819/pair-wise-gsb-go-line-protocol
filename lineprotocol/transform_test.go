package lineprotocol

import (
	"errors"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
)

// redactTransform returns a transform that replaces every whitelisted
// value with a fixed string.
func redactTransform(replacement string) FieldTransformFunc {
	return func(key []byte, v Value) (Value, error) {
		return MustNewValue(replacement), nil
	}
}

// assertOnlyRangeChanged asserts that out differs from line only within
// the byte range [start, end), proving that the transformation did not
// touch any bytes outside the replaced field value fragment.
func assertOnlyRangeChanged(c *qt.C, out, line []byte, start, end int, replacement string) {
	c.Assert(string(out[:start]), qt.Equals, string(line[:start]), qt.Commentf("prefix before replaced value changed"))
	c.Assert(string(out[start:start+len(replacement)]), qt.Equals, replacement)
	c.Assert(string(out[start+len(replacement):]), qt.Equals, string(line[end:]), qt.Commentf("suffix after replaced value changed"))
	c.Assert(len(out), qt.Equals, len(line)-(end-start)+len(replacement))
}

func TestTransformFieldsPreservesEverythingButValue(t *testing.T) {
	c := qt.New(t)
	line := `cpu,host=server\ 1,region=us\ west user="redact me",load=42i,temp=98.60,ok=true 1622540400000000000` + "\n"
	out, err := TransformFields([]byte(line), []string{"user"}, redactTransform("[REDACTED]"))
	c.Assert(err, qt.IsNil)

	start := strings.Index(line, `"redact me"`)
	end := start + len(`"redact me"`)
	assertOnlyRangeChanged(c, out, []byte(line), start, end, `"[REDACTED]"`)
	c.Assert(string(out), qt.Equals, `cpu,host=server\ 1,region=us\ west user="[REDACTED]",load=42i,temp=98.60,ok=true 1622540400000000000`+"\n")
}

func TestTransformFieldsStringEscaping(t *testing.T) {
	c := qt.New(t)
	// The replacement string contains a comma, a backslash, a quote, a
	// newline and something that looks like a timestamp. All of it must
	// be encoded as a plain string value without breaking the line.
	replacement := "a,b\\c\"d\ne 1622540400000000000"
	line := `m s="original",n=5i 1622540400000000000`
	out, err := TransformFields([]byte(line), []string{"s"}, func(key []byte, v Value) (Value, error) {
		c.Assert(string(key), qt.Equals, "s")
		c.Assert(v.Kind(), qt.Equals, String)
		c.Assert(v.StringV(), qt.Equals, "original")
		return MustNewValue(replacement), nil
	})
	c.Assert(err, qt.IsNil)

	// The comma and the timestamp lookalike need no escaping inside a
	// string value; the backslash, quote and newline do.
	expect := `m s="a,b\\c\"d\ne 1622540400000000000",n=5i 1622540400000000000`
	c.Assert(string(out), qt.Equals, expect)

	start := strings.Index(line, `"original"`)
	end := start + len(`"original"`)
	assertOnlyRangeChanged(c, out, []byte(line), start, end, `"a,b\\c\"d\ne 1622540400000000000"`)

	// The output must still decode as exactly one valid point holding
	// the replacement value and the untouched timestamp.
	dec := NewDecoderWithBytes(out)
	c.Assert(dec.Next(), qt.IsTrue)
	measure, err := dec.Measurement()
	c.Assert(err, qt.IsNil)
	c.Assert(string(measure), qt.Equals, "m")
	key, v, err := dec.NextField()
	c.Assert(err, qt.IsNil)
	c.Assert(string(key), qt.Equals, "s")
	c.Assert(v.StringV(), qt.Equals, replacement)
	key, v, err = dec.NextField()
	c.Assert(err, qt.IsNil)
	c.Assert(string(key), qt.Equals, "n")
	c.Assert(v.IntV(), qt.Equals, int64(5))
	key, _, err = dec.NextField()
	c.Assert(err, qt.IsNil)
	c.Assert(key, qt.IsNil)
	ts, err := dec.Time(Nanosecond, time.Time{})
	c.Assert(err, qt.IsNil)
	c.Assert(ts.UnixNano(), qt.Equals, int64(1622540400000000000))
	c.Assert(dec.Next(), qt.IsFalse)
}

func TestTransformFieldsTimestampLikeStringUntouched(t *testing.T) {
	c := qt.New(t)
	// A string field whose content looks exactly like a timestamp must
	// not be confused with the actual timestamp, and the timestamp
	// bytes must stay identical.
	line := `m s="1622540400000000000",t=1i 1622540400000000000`
	out, err := TransformFields([]byte(line), []string{"t"}, func(key []byte, v Value) (Value, error) {
		return IntValue(2), nil
	})
	c.Assert(err, qt.IsNil)
	c.Assert(string(out), qt.Equals, `m s="1622540400000000000",t=2i 1622540400000000000`)
}

func TestTransformFieldsUnmatchedFieldsKeepRawBytes(t *testing.T) {
	c := qt.New(t)
	// Non-whitelisted fields keep their exact original formatting,
	// even when it isn't the canonical encoding.
	line := `m x=1.50,y=007i,z=true,w="keep,me" 99`
	out, err := TransformFields([]byte(line), []string{"absent"}, redactTransform("x"))
	c.Assert(err, qt.IsNil)
	c.Assert(string(out), qt.Equals, line)
}

func TestTransformFieldsDuplicateKeysEachOccurrence(t *testing.T) {
	c := qt.New(t)
	// Duplicate whitelisted keys: every occurrence is transformed in
	// order of appearance; the last one does not silently win.
	line := `m x="a",y=1i,x="b" 7`
	var got []string
	out, err := TransformFields([]byte(line), []string{"x"}, func(key []byte, v Value) (Value, error) {
		got = append(got, v.StringV())
		return MustNewValue("[" + v.StringV() + "]"), nil
	})
	c.Assert(err, qt.IsNil)
	c.Assert(got, qt.DeepEquals, []string{"a", "b"})
	c.Assert(string(out), qt.Equals, `m x="[a]",y=1i,x="[b]" 7`)
}

func TestTransformFieldsMatchesDecodedKey(t *testing.T) {
	c := qt.New(t)
	// The whitelist matches the decoded (unescaped) field key.
	line := `m my\ key="v",other="w"`
	out, err := TransformFields([]byte(line), []string{"my key"}, redactTransform("x"))
	c.Assert(err, qt.IsNil)
	c.Assert(string(out), qt.Equals, `m my\ key="x",other="w"`)
}

func TestTransformFieldsKinds(t *testing.T) {
	c := qt.New(t)
	line := `m i=42i,u=43u,f=1.5,b=true,s="x" 5`
	out, err := TransformFields([]byte(line), []string{"i", "u", "f", "b", "s"}, func(key []byte, v Value) (Value, error) {
		switch string(key) {
		case "i":
			c.Assert(v.Kind(), qt.Equals, Int)
			return IntValue(v.IntV() + 1), nil
		case "u":
			c.Assert(v.Kind(), qt.Equals, Uint)
			return UintValue(v.UintV() + 1), nil
		case "f":
			c.Assert(v.Kind(), qt.Equals, Float)
			newv, ok := FloatValue(v.FloatV() + 1)
			if !ok {
				return Value{}, errors.New("invalid float")
			}
			return newv, nil
		case "b":
			c.Assert(v.Kind(), qt.Equals, Bool)
			return BoolValue(!v.BoolV()), nil
		case "s":
			c.Assert(v.Kind(), qt.Equals, String)
			return MustNewValue("y"), nil
		}
		panic("unreachable")
	})
	c.Assert(err, qt.IsNil)
	c.Assert(string(out), qt.Equals, `m i=43i,u=44u,f=2.5,b=false,s="y" 5`)
}

func TestTransformFieldsErrorCases(t *testing.T) {
	c := qt.New(t)
	callbackErr := errors.New("callback failed")
	tests := []struct {
		name    string
		line    string
		fields  []string
		f       FieldTransformFunc
		wantErr string
	}{{
		name:    "empty input",
		line:    "",
		fields:  []string{"x"},
		f:       redactTransform("y"),
		wantErr: "no line-protocol entry found",
	}, {
		name:    "comment only",
		line:    "# just a comment\n",
		fields:  []string{"x"},
		f:       redactTransform("y"),
		wantErr: "no line-protocol entry found",
	}, {
		name:    "syntax error before field",
		line:    `m x=`,
		fields:  []string{"x"},
		f:       redactTransform("y"),
		wantErr: `at line 1:\d+: expected value for field "x", found end of input`,
	}, {
		name:    "syntax error after fields",
		line:    `m x="a" 123 trailing`,
		fields:  []string{"x"},
		f:       redactTransform("y"),
		wantErr: `at line 1:\d+: unexpected text after timestamp \("trailing"\)`,
	}, {
		name:    "unparseable whitelisted value",
		line:    `m x=99999999999999999999999999i`,
		fields:  []string{"x"},
		f:       redactTransform("y"),
		wantErr: `cannot parse value for field "x": line-protocol value out of range`,
	}, {
		name:   "callback error",
		line:   `m x="a"`,
		fields: []string{"x"},
		f: func(key []byte, v Value) (Value, error) {
			return Value{}, callbackErr
		},
		wantErr: `cannot transform field "x": callback failed`,
	}, {
		name:    "multiple entries",
		line:    "m x=1i\nm2 y=2i\n",
		fields:  []string{"x"},
		f:       redactTransform("y"),
		wantErr: "found more than one line-protocol entry",
	}}
	for _, test := range tests {
		c.Run(test.name, func(c *qt.C) {
			out, err := TransformFields([]byte(test.line), test.fields, test.f)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			// A failed transformation must not produce any output.
			c.Assert(out, qt.IsNil)
		})
	}
}

func TestTransformFieldsCallbackErrorAfterFirstReplacement(t *testing.T) {
	c := qt.New(t)
	// Even when an earlier field was already transformed successfully,
	// a later failure must discard the whole line.
	line := `m x="a",y="b"`
	out, err := TransformFields([]byte(line), []string{"x", "y"}, func(key []byte, v Value) (Value, error) {
		if string(key) == "y" {
			return Value{}, errors.New("boom")
		}
		return MustNewValue("z"), nil
	})
	c.Assert(err, qt.ErrorMatches, `cannot transform field "y": boom`)
	c.Assert(out, qt.IsNil)
}
