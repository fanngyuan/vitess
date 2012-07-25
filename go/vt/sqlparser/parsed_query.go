// Copyright 2012, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sqlparser

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

type TrackedBuffer struct {
	*bytes.Buffer
	bind_locations []BindLocation
}

type BindLocation struct {
	Offset, Length int
}

func NewTrackedBuffer() *TrackedBuffer {
	return &TrackedBuffer{bytes.NewBuffer(make([]byte, 0, 128)), make([]BindLocation, 0, 4)}
}

type ParsedQuery struct {
	Query         string
	BindLocations []BindLocation
}

func NewParsedQuery(buf *TrackedBuffer) *ParsedQuery {
	return &ParsedQuery{buf.String(), buf.bind_locations}
}

type EncoderFunc func(value interface{}) ([]byte, error)

func (self *ParsedQuery) GenerateQuery(bindVariables map[string]interface{}, listVariables []interface{}) ([]byte, error) {
	if bindVariables == nil || len(self.BindLocations) == 0 {
		return []byte(self.Query), nil
	}
	buf := bytes.NewBuffer(make([]byte, 0, len(self.Query)))
	current := 0
	for _, loc := range self.BindLocations {
		buf.WriteString(self.Query[current:loc.Offset])
		varName := self.Query[loc.Offset+1 : loc.Offset+loc.Length]
		var supplied interface{}
		if varName[0] >= '0' && varName[0] <= '9' {
			index, err := strconv.Atoi(varName)
			if err != nil {
				return nil, NewParserError("Unexpected: %v for %s", err, varName)
			}
			if index >= len(listVariables) {
				return nil, NewParserError("Index out of range: %d", index)
			}
			supplied = listVariables[index]
		} else {
			var ok bool
			supplied, ok = bindVariables[varName]
			if !ok {
				return nil, NewParserError("Bind variable %s not found", varName)
			}
		}
		if err := EncodeValue(buf, supplied); err != nil {
			return nil, err
		}
		current = loc.Offset + loc.Length
	}
	buf.WriteString(self.Query[current:])
	return buf.Bytes(), nil
}

func (self *ParsedQuery) MarshalJSON() ([]byte, error) {
	return json.Marshal(self.Query)
}

func EncodeValue(buf *bytes.Buffer, value interface{}) error {
	switch bindVal := value.(type) {
	case nil:
		buf.WriteString("NULL")
	case int:
		buf.WriteString(strconv.FormatInt(int64(bindVal), 10))
	case int32:
		buf.WriteString(strconv.FormatInt(int64(bindVal), 10))
	case int64:
		buf.WriteString(strconv.FormatInt(int64(bindVal), 10))
	case uint:
		buf.WriteString(strconv.FormatUint(uint64(bindVal), 10))
	case uint32:
		buf.WriteString(strconv.FormatUint(uint64(bindVal), 10))
	case uint64:
		buf.WriteString(strconv.FormatUint(uint64(bindVal), 10))
	case float64:
		buf.WriteString(strconv.FormatFloat(bindVal, 'f', -1, 64))
	case string:
		EncodeBinary(buf, []byte(bindVal))
	case []byte:
		EncodeBinary(buf, bindVal)
	case time.Time:
		buf.WriteString(bindVal.Format("'2006-01-02 15:04:05'"))
	case []interface{}:
		for i := 0; i < len(bindVal); i++ {
			if i != 0 {
				buf.WriteString(", ")
			}
			if err := EncodeValue(buf, bindVal[i]); err != nil {
				return err
			}
		}
	case [][]interface{}:
		for i := 0; i < len(bindVal); i++ {
			if i != 0 {
				buf.WriteString(", ")
			}
			buf.WriteByte('(')
			if err := EncodeValue(buf, bindVal[i]); err != nil {
				return err
			}
			buf.WriteByte(')')
		}
	default:
		return errors.New(fmt.Sprintf("Bad bind variable type %T", value))
	}
	return nil
}

func EncodeBinary(buf *bytes.Buffer, bytes []byte) {
	buf.WriteByte('\'')
	for _, ch := range bytes {
		if encodedChar, ok := escapeEncodeMap[ch]; ok {
			buf.WriteByte('\\')
			buf.WriteByte(encodedChar)
		} else {
			buf.WriteByte(ch)
		}
	}
	buf.WriteByte('\'')
}
