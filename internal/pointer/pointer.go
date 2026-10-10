package pointer

import (
	"errors"
	"strconv"
	"strings"
)

type Pointer struct {
	text   string
	tokens []string
}

func Parse(text string) (Pointer, error) {
	if text == "" {
		return Pointer{}, nil
	}
	if !strings.HasPrefix(text, "/") {
		return Pointer{}, errors.New("a JSON pointer is empty or starts with /, as RFC 6901 writes it: " + text)
	}
	parts := strings.Split(text[1:], "/")
	tokens := make([]string, len(parts))
	for i, part := range parts {
		token, err := unescape(part)
		if err != nil {
			return Pointer{}, err
		}
		tokens[i] = token
	}
	return Pointer{text: text, tokens: tokens}, nil
}

func unescape(part string) (string, error) {
	if !strings.Contains(part, "~") {
		return part, nil
	}
	var builder strings.Builder
	for i := 0; i < len(part); i++ {
		if part[i] != '~' {
			builder.WriteByte(part[i])
			continue
		}
		if i+1 == len(part) || (part[i+1] != '0' && part[i+1] != '1') {
			return "", errors.New("a JSON pointer writes ~ as ~0 and / as ~1: " + part)
		}
		if part[i+1] == '0' {
			builder.WriteByte('~')
		} else {
			builder.WriteByte('/')
		}
		i++
	}
	return builder.String(), nil
}

func (p Pointer) String() string {
	return p.text
}

func (p Pointer) Find(document any) (any, bool) {
	current := document
	for _, token := range p.tokens {
		switch node := current.(type) {
		case map[string]any:
			next, ok := node[token]
			if !ok {
				return nil, false
			}
			current = next
		case []any:
			index, ok := arrayIndex(token, len(node))
			if !ok {
				return nil, false
			}
			current = node[index]
		default:
			return nil, false
		}
	}
	return current, true
}

func arrayIndex(token string, length int) (int, bool) {
	if token == "" || (len(token) > 1 && token[0] == '0') {
		return 0, false
	}
	for _, character := range token {
		if character < '0' || character > '9' {
			return 0, false
		}
	}
	index, err := strconv.Atoi(token)
	if err != nil || index >= length {
		return 0, false
	}
	return index, true
}
