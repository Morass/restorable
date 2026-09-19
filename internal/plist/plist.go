// Package plist reads Apple property lists. Binary files are turned into XML by
// plutil, and the XML is parsed here — plutil's own JSON conversion cannot be
// used, because it refuses any plist that holds a date, and Time Machine's
// preferences are full of dates.
package plist

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/morass/restorable/internal/run"
)

// Value is a parsed plist value: Dict, Array, string, int64, float64, bool,
// time.Time or []byte.
type Value any

// Dict is a plist dictionary.
type Dict map[string]Value

// Array is a plist array.
type Array []Value

// FromFile reads a plist file of any format by asking plutil for XML first.
func FromFile(ctx context.Context, r run.Runner, path string) (Value, error) {
	res, err := r.Run(ctx, run.Plutil, "-convert", "xml1", "-o", "-", "--", path)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("plutil %s: %s", path, firstLine(res.Stderr))
	}
	return Parse(res.Stdout)
}

// Parse reads an XML property list.
func Parse(data []byte) (Value, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil, fmt.Errorf("plist: no value found")
		}
		if err != nil {
			return nil, fmt.Errorf("plist: %w", err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "plist" {
			continue
		}
		v, err := parseNextValue(dec)
		if err != nil {
			return nil, err
		}
		return v, nil
	}
}

// parseNextValue reads the next value element, skipping whitespace and comments.
func parseNextValue(dec *xml.Decoder) (Value, error) {
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			return parseValue(dec, t)
		case xml.EndElement:
			return nil, nil // an empty container
		}
	}
}

func parseValue(dec *xml.Decoder, start xml.StartElement) (Value, error) {
	switch start.Name.Local {
	case "dict":
		return parseDict(dec)
	case "array":
		return parseArray(dec)
	case "true":
		return true, dec.Skip()
	case "false":
		return false, dec.Skip()
	case "string", "key":
		s, err := text(dec, start)
		return s, err
	case "integer":
		s, err := text(dec, start)
		if err != nil {
			return nil, err
		}
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("plist: %q is not an integer", s)
		}
		return n, nil
	case "real":
		s, err := text(dec, start)
		if err != nil {
			return nil, err
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return nil, fmt.Errorf("plist: %q is not a real", s)
		}
		return f, nil
	case "date":
		s, err := text(dec, start)
		if err != nil {
			return nil, err
		}
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(s))
		if err != nil {
			return nil, fmt.Errorf("plist: %q is not a date", s)
		}
		return t, nil
	case "data":
		s, err := text(dec, start)
		if err != nil {
			return nil, err
		}
		raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), ""))
		if err != nil {
			return nil, fmt.Errorf("plist: unreadable data: %w", err)
		}
		return raw, nil
	default:
		// An element we do not know is skipped rather than failing the file.
		return nil, dec.Skip()
	}
}

func parseDict(dec *xml.Decoder) (Dict, error) {
	d := Dict{}
	var key string
	haveKey := false
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "key" {
				k, err := text(dec, t)
				if err != nil {
					return nil, err
				}
				key, haveKey = k, true
				continue
			}
			v, err := parseValue(dec, t)
			if err != nil {
				return nil, err
			}
			if haveKey {
				d[key] = v
				haveKey = false
			}
		case xml.EndElement:
			if t.Name.Local == "dict" {
				return d, nil
			}
		}
	}
}

func parseArray(dec *xml.Decoder) (Array, error) {
	var a Array
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			v, err := parseValue(dec, t)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		case xml.EndElement:
			if t.Name.Local == "array" {
				return a, nil
			}
		}
	}
}

func text(dec *xml.Decoder, start xml.StartElement) (string, error) {
	var b strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.CharData:
			b.Write(t)
		case xml.EndElement:
			if t.Name.Local == start.Name.Local {
				return b.String(), nil
			}
		}
	}
}

// AsDict reads a value as a dictionary.
func AsDict(v Value) (Dict, bool) { d, ok := v.(Dict); return d, ok }

// Get walks nested dictionaries: Get(v, "Destinations") and so on.
func Get(v Value, key string) (Value, bool) {
	d, ok := AsDict(v)
	if !ok {
		return nil, false
	}
	got, ok := d[key]
	return got, ok
}

// Str reads a string value under a key.
func Str(v Value, key string) string {
	got, ok := Get(v, key)
	if !ok {
		return ""
	}
	s, _ := got.(string)
	return s
}

// Time reads a date under a key, accepting the string forms Apple also writes.
func Time(v Value, key string) (time.Time, bool) {
	got, ok := Get(v, key)
	if !ok {
		return time.Time{}, false
	}
	switch t := got.(type) {
	case time.Time:
		return t, true
	case string:
		for _, layout := range []string{time.RFC3339, "2006-01-02-150405", "2006-01-02 15:04:05 -0700"} {
			if parsed, err := time.Parse(layout, strings.TrimSpace(t)); err == nil {
				return parsed, true
			}
		}
	}
	return time.Time{}, false
}

// Times reads an array of dates under a key, ignoring entries that are not dates.
func Times(v Value, key string) []time.Time {
	got, ok := Get(v, key)
	if !ok {
		return nil
	}
	arr, ok := got.(Array)
	if !ok {
		return nil
	}
	var out []time.Time
	for _, item := range arr {
		switch t := item.(type) {
		case time.Time:
			out = append(out, t)
		case string:
			if parsed, err := time.Parse("2006-01-02-150405", strings.TrimSpace(t)); err == nil {
				out = append(out, parsed)
			}
		}
	}
	return out
}

// Dicts reads an array of dictionaries under a key.
func Dicts(v Value, key string) []Dict {
	got, ok := Get(v, key)
	if !ok {
		return nil
	}
	arr, ok := got.(Array)
	if !ok {
		return nil
	}
	var out []Dict
	for _, item := range arr {
		if d, ok := AsDict(item); ok {
			out = append(out, d)
		}
	}
	return out
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
