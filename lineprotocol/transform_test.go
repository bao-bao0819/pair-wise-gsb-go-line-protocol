package lineprotocol

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
)

// stringReplacer returns a FieldReplaceFunc that replaces every
// whitelisted value with a string value holding s.
func stringReplacer(s string) FieldReplaceFunc {
	return func(key []byte, value Value) (Value, error) {
		return MustNewValue(s), nil
	}
}

// assertSplice asserts that out is exactly line with the byte range
// [start, end) replaced by repl, and that nothing outside that range
// has been modified.
func assertSplice(c *qt.C, line, out string, start, end int, repl string) {
	c.Assert(out[:start], qt.Equals, line[:start], qt.Commentf("prefix before replaced range differs"))
	c.Assert(out[start:start+len(repl)], qt.Equals, repl, qt.Commentf("replaced segment differs"))
	c.Assert(out[start+len(repl):], qt.Equals, line[end:], qt.Commentf("suffix after replaced range differs"))
	c.Assert(len(out), qt.Equals, len(line)-(end-start)+len(repl))
}

// assertDecodesTo asserts that data holds exactly one valid
// line-protocol entry with the given measurement, tags, fields and
// timestamp bytes.
func assertDecodesTo(c *qt.C, data []byte, measurement string, tags map[string]string, fields map[string]Value, timestamp string) {
	d := NewDecoderWithBytes(data)
	c.Assert(d.Next(), qt.IsTrue)
	m, err := d.Measurement()
	c.Assert(err, qt.IsNil)
	c.Assert(string(m), qt.Equals, measurement)
	gotTags := make(map[string]string)
	for {
		k, v, err := d.NextTag()
		c.Assert(err, qt.IsNil)
		if k == nil {
			break
		}
		gotTags[string(k)] = string(v)
	}
	c.Assert(gotTags, qt.DeepEquals, tags)
	gotFields := make(map[string]Value)
	for {
		k, v, err := d.NextField()
		c.Assert(err, qt.IsNil)
		if k == nil {
			break
		}
		gotFields[string(k)] = v
	}
	c.Assert(gotFields, qt.DeepEquals, fields)
	ts, err := d.TimeBytes()
	c.Assert(err, qt.IsNil)
	if timestamp == "" {
		c.Assert(ts, qt.IsNil)
	} else {
		c.Assert(string(ts), qt.Equals, timestamp)
	}
	c.Assert(d.Next(), qt.IsFalse, qt.Commentf("more than one entry in %q", data))
}

func TestReplaceFieldsBasic(t *testing.T) {
	c := qt.New(t)
	line := `cpu,host=a,region=us\ west load=15.6,secret="hunter2",count=42i 1600000000000000000` + "\n"
	out, err := ReplaceFields([]byte(line), []string{"secret"}, stringReplacer("REDACTED"))
	c.Assert(err, qt.IsNil)

	start := strings.Index(line, `"hunter2"`)
	c.Assert(start >= 0, qt.IsTrue)
	assertSplice(c, line, string(out), start, start+len(`"hunter2"`), `"REDACTED"`)

	// Measurement, tags, other fields, field order and timestamp are
	// all preserved byte-for-byte.
	assertDecodesTo(c, out, "cpu",
		map[string]string{"host": "a", "region": "us west"},
		map[string]Value{
			"load":   MustNewValue(15.6),
			"secret": MustNewValue("REDACTED"),
			"count":  MustNewValue(int64(42)),
		},
		"1600000000000000000",
	)
}

func TestReplaceFieldsTrickyStringContent(t *testing.T) {
	c := qt.New(t)
	// The string value contains a comma, a backslash, an escaped quote
	// and content that looks like a timestamp. None of that must
	// confuse the scanner, and the real timestamp must stay intact.
	line := `m f="a,b\\c\"d 1600000000000000000",g=1i 9999999999999999999` + "\n"
	var gotValue Value
	out, err := ReplaceFields([]byte(line), []string{"f"}, func(key []byte, value Value) (Value, error) {
		gotValue = value
		return MustNewValue("x"), nil
	})
	c.Assert(err, qt.IsNil)

	// The callback receives the decoded value, not the raw syntax.
	c.Assert(gotValue.Kind(), qt.Equals, String)
	c.Assert(gotValue.StringV(), qt.Equals, `a,b\c"d 1600000000000000000`)

	start := strings.Index(line, `"a,b`)
	end := strings.Index(line, `",g=1i`) + 1
	c.Assert(start >= 0 && end > start, qt.IsTrue)
	assertSplice(c, line, string(out), start, end, `"x"`)

	assertDecodesTo(c, out, "m",
		map[string]string{},
		map[string]Value{"f": MustNewValue("x"), "g": MustNewValue(int64(1))},
		"9999999999999999999",
	)
}

func TestReplaceFieldsReplacementIsEncoded(t *testing.T) {
	c := qt.New(t)
	line := `m f="old",g=2i 100` + "\n"
	// The replacement contains characters that would corrupt the line
	// if spliced in raw: quotes, a newline, a backslash and a comma.
	repl := "quo\"te\nnew\\line,"
	out, err := ReplaceFields([]byte(line), []string{"f"}, stringReplacer(repl))
	c.Assert(err, qt.IsNil)

	// The only newline in the output is the trailing one.
	c.Assert(strings.Count(string(out), "\n"), qt.Equals, 1)
	c.Assert(strings.HasSuffix(string(out), "\n"), qt.IsTrue)

	start := strings.Index(line, `"old"`)
	assertSplice(c, line, string(out), start, start+len(`"old"`), `"quo\"te\nnew\\line,"`)

	// The output still decodes as a single valid point and the value
	// round-trips exactly.
	assertDecodesTo(c, out, "m",
		map[string]string{},
		map[string]Value{"f": MustNewValue(repl), "g": MustNewValue(int64(2))},
		"100",
	)
}

func TestReplaceFieldsDuplicateFields(t *testing.T) {
	c := qt.New(t)
	line := `m f="a",f="b",f="c"` + "\n"
	var calls []string
	out, err := ReplaceFields([]byte(line), []string{"f"}, func(key []byte, value Value) (Value, error) {
		calls = append(calls, value.StringV())
		return MustNewValue(strings.ToUpper(value.StringV())), nil
	})
	c.Assert(err, qt.IsNil)
	// Each occurrence is visited and replaced independently - it's
	// never the case that only the last one is changed.
	c.Assert(calls, qt.DeepEquals, []string{"a", "b", "c"})
	c.Assert(string(out), qt.Equals, `m f="A",f="B",f="C"`+"\n")
}

func TestReplaceFieldsMatchesDecodedKey(t *testing.T) {
	c := qt.New(t)
	// The raw key is escaped; the whitelist holds the decoded name.
	line := `m sec\ ret\ key="v",secretkey="w"` + "\n"
	out, err := ReplaceFields([]byte(line), []string{"sec ret key"}, stringReplacer("x"))
	c.Assert(err, qt.IsNil)
	c.Assert(string(out), qt.Equals, `m sec\ ret\ key="x",secretkey="w"`+"\n")
}

func TestReplaceFieldsKindChange(t *testing.T) {
	c := qt.New(t)
	line := `m a=1i,b="x",c=T,d=10u,e=1.5e-3 42` + "\n"
	out, err := ReplaceFields([]byte(line), []string{"a", "b"}, func(key []byte, value Value) (Value, error) {
		switch string(key) {
		case "a":
			c.Assert(value.Kind(), qt.Equals, Int)
			return MustNewValue("s"), nil
		case "b":
			c.Assert(value.Kind(), qt.Equals, String)
			return MustNewValue(int64(2)), nil
		}
		return Value{}, fmt.Errorf("unexpected field %q", key)
	})
	c.Assert(err, qt.IsNil)
	// Non-matching fields keep their original bytes exactly, including
	// bool, uint and float spellings.
	c.Assert(string(out), qt.Equals, `m a="s",b=2i,c=T,d=10u,e=1.5e-3 42`+"\n")
	assertDecodesTo(c, out, "m",
		map[string]string{},
		map[string]Value{
			"a": MustNewValue("s"),
			"b": MustNewValue(int64(2)),
			"c": MustNewValue(true),
			"d": MustNewValue(uint64(10)),
			"e": MustNewValue(1.5e-3),
		},
		"42",
	)
}

func TestReplaceFieldsNoMatch(t *testing.T) {
	c := qt.New(t)
	line := `m,tag=v f="a",g=1i 5` + "\n"
	out, err := ReplaceFields([]byte(line), []string{"nope"}, stringReplacer("x"))
	c.Assert(err, qt.IsNil)
	c.Assert(string(out), qt.Equals, line)
}

func TestReplaceFieldsLeadingCommentsAndBlankLines(t *testing.T) {
	c := qt.New(t)
	line := "# a comment with spaces and \"quotes\"\n\n  m f=\"a\" 7\n"
	out, err := ReplaceFields([]byte(line), []string{"f"}, stringReplacer("b"))
	c.Assert(err, qt.IsNil)
	c.Assert(string(out), qt.Equals, "# a comment with spaces and \"quotes\"\n\n  m f=\"b\" 7\n")
}

func TestReplaceFieldsCallbackErrorFailsWholeLine(t *testing.T) {
	c := qt.New(t)
	line := `m f="a",g="b" 1` + "\n"
	boom := errors.New("boom")
	calls := 0
	_, err := ReplaceFields([]byte(line), []string{"f", "g"}, func(key []byte, value Value) (Value, error) {
		calls++
		if calls == 2 {
			return Value{}, boom
		}
		return MustNewValue("x"), nil
	})
	// Even though the first field was replaced successfully, the line
	// as a whole fails and no partial output is produced.
	c.Assert(err, qt.ErrorMatches, `lineprotocol: cannot replace value for field "g": boom`)
	c.Assert(errors.Is(err, boom), qt.IsTrue)
}

func TestReplaceFieldsUnparseableMatchingValueFails(t *testing.T) {
	c := qt.New(t)
	// The integer value is out of range. When the field matches the
	// whitelist, the whole line fails...
	line := `m f=99999999999999999999999999i,g=1i` + "\n"
	_, err := ReplaceFields([]byte(line), []string{"f"}, stringReplacer("x"))
	c.Assert(err, qt.ErrorMatches, `lineprotocol: cannot parse value for field "f": .*`)
	c.Assert(errors.Is(err, ErrValueOutOfRange), qt.IsTrue)

	// ...but when it doesn't match, its bytes pass through untouched.
	out, err := ReplaceFields([]byte(line), []string{"g"}, stringReplacer("x"))
	c.Assert(err, qt.IsNil)
	c.Assert(string(out), qt.Equals, `m f=99999999999999999999999999i,g="x"`+"\n")
}

func TestReplaceFieldsSyntaxErrors(t *testing.T) {
	c := qt.New(t)
	for _, line := range []string{
		"",
		"\n",
		"m",
		"m f=",
		"m f=\"unterminated",
		"m ,f=1i",
		"m f=1i\nm2 f=2i\n",
		"m f=1i trailing junk\n",
	} {
		_, err := ReplaceFields([]byte(line), []string{"f"}, stringReplacer("x"))
		c.Assert(err, qt.Not(qt.IsNil), qt.Commentf("line %q should fail", line))
	}
}

func TestReplaceFieldsNilReplaceFunc(t *testing.T) {
	c := qt.New(t)
	_, err := ReplaceFields([]byte("m f=1i\n"), []string{"f"}, nil)
	c.Assert(err, qt.Not(qt.IsNil))
}

func TestReplaceFieldsStringSpanningNewline(t *testing.T) {
	c := qt.New(t)
	// A string field value may itself contain a newline; the
	// replacement must still only touch the value's byte range.
	line := "m f=\"line1\nline2\",g=1i 5\n"
	out, err := ReplaceFields([]byte(line), []string{"f"}, stringReplacer("z"))
	c.Assert(err, qt.IsNil)
	c.Assert(string(out), qt.Equals, "m f=\"z\",g=1i 5\n")
	assertDecodesTo(c, out, "m",
		map[string]string{},
		map[string]Value{"f": MustNewValue("z"), "g": MustNewValue(int64(1))},
		"5",
	)
}
