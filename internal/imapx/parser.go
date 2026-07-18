// MailFerry — IMAP Migration & Sync
// A High-Performance Native IMAP Migration Engine
//
// Copyright (C) 2026 Andy Saputra
// Author: Andy Saputra <andy@saputra.org>
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// This file is part of MailFerry (https://github.com/ajsap/mailferry).
// Licensed under the GNU Affero General Public License v3.0 or later;
// see the LICENSE file for details.

package imapx

// IMAP response tokenizer. A logical line (small literals already inlined
// as []byte tokens by the reader) becomes a token tree:
//   string       atom / quoted string / number
//   []byte       literal
//   []any        parenthesised list
// plus the response envelope (tag, status/type, number, code, text).

import (
	"fmt"
	"strconv"
	"strings"
)

type Response struct {
	Tag    string // "*", "+" or the command tag
	Status string // OK NO BAD PREAUTH BYE (when a status response)
	Type   string // EXISTS FETCH LIST SEARCH STATUS CAPABILITY FLAGS RECENT EXPUNGE ...
	Num    uint32 // leading number for "* 12 EXISTS" / "* 3 FETCH"
	Tokens []any  // remaining tokens after envelope
	Code   []any  // [CODE ...] response code content
	Text   string // trailing human text
	Raw    string
}

func (r *Response) IsStatus() bool { return r.Status != "" }

// tokenize parses the token portion of a line. segs carries the pieces
// split at inlined literal boundaries; lits the literal bytes between them.
func tokenize(segs []string, lits [][]byte) []any {
	type frame struct{ items []any }
	stack := []*frame{{}}
	push := func(v any) { f := stack[len(stack)-1]; f.items = append(f.items, v) }
	for si, seg := range segs {
		i := 0
		for i < len(seg) {
			c := seg[i]
			switch {
			case c == ' ':
				i++
			case c == '(':
				stack = append(stack, &frame{})
				i++
			case c == ')':
				if len(stack) > 1 {
					f := stack[len(stack)-1]
					stack = stack[:len(stack)-1]
					push(f.items)
				}
				i++
			case c == '"':
				j := i + 1
				var b strings.Builder
				for j < len(seg) {
					if seg[j] == '\\' && j+1 < len(seg) {
						b.WriteByte(seg[j+1])
						j += 2
						continue
					}
					if seg[j] == '"' {
						break
					}
					b.WriteByte(seg[j])
					j++
				}
				push(b.String())
				i = j + 1
			case c == '[':
				// bracketed body-section atom, e.g. BODY[HEADER.FIELDS (...)]
				depth := 0
				j := i
				for j < len(seg) {
					if seg[j] == '[' {
						depth++
					} else if seg[j] == ']' {
						depth--
						if depth == 0 {
							j++
							break
						}
					}
					j++
				}
				push(seg[i:j])
				i = j
			default:
				j := i
				for j < len(seg) && seg[j] != ' ' && seg[j] != '(' && seg[j] != ')' {
					if seg[j] == '[' { // attach bracket to preceding atom (BODY[...])
						depth := 0
						for j < len(seg) {
							if seg[j] == '[' {
								depth++
							} else if seg[j] == ']' {
								depth--
								if depth == 0 {
									j++
									break
								}
							}
							j++
						}
						continue
					}
					j++
				}
				push(seg[i:j])
				i = j
			}
		}
		if si < len(lits) {
			push(lits[si])
		}
	}
	return stack[0].items
}

var statusWords = map[string]bool{
	"OK": true, "NO": true, "BAD": true, "PREAUTH": true, "BYE": true,
}

// ParseLine builds a Response from a logical line.
func ParseLine(segs []string, lits [][]byte) (*Response, error) {
	if len(segs) == 0 {
		return nil, fmt.Errorf("empty response")
	}
	first := segs[0]
	r := &Response{Raw: first}
	sp := strings.IndexByte(first, ' ')
	if sp < 0 {
		if first == "+" {
			return &Response{Tag: "+"}, nil
		}
		return nil, fmt.Errorf("malformed response line: %q", first)
	}
	r.Tag = first[:sp]
	rest := first[sp+1:]
	if r.Tag == "+" {
		r.Text = rest
		return r, nil
	}

	// optional leading number: "* 12 EXISTS"
	sp2 := strings.IndexByte(rest, ' ')
	head := rest
	if sp2 >= 0 {
		head = rest[:sp2]
	}
	if n, err := strconv.ParseUint(head, 10, 32); err == nil && r.Tag == "*" {
		r.Num = uint32(n)
		if sp2 < 0 {
			return r, nil
		}
		rest = rest[sp2+1:]
		sp2 = strings.IndexByte(rest, ' ')
		head = rest
		if sp2 >= 0 {
			head = rest[:sp2]
		}
	}

	word := strings.ToUpper(head)
	if statusWords[word] {
		r.Status = word
		text := ""
		if sp2 >= 0 {
			text = rest[sp2+1:]
		}
		// [CODE args] prefix
		if strings.HasPrefix(text, "[") {
			if end := strings.IndexByte(text, ']'); end > 0 {
				code := text[1:end]
				r.Code = tokenize([]string{code}, nil)
				text = strings.TrimPrefix(text[end+1:], " ")
			}
		}
		r.Text = text
		return r, nil
	}

	r.Type = word
	tokRest := ""
	if sp2 >= 0 {
		tokRest = rest[sp2+1:]
	}
	all := append([]string{tokRest}, segs[1:]...)
	r.Tokens = tokenize(all, lits)
	return r, nil
}

// Helpers over token trees.

func TokStr(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case []byte:
		return string(x), true
	}
	return "", false
}

func TokUint(v any) (uint32, bool) {
	if s, ok := TokStr(v); ok {
		if n, err := strconv.ParseUint(s, 10, 32); err == nil {
			return uint32(n), true
		}
	}
	return 0, false
}

func TokInt64(v any) (int64, bool) {
	if s, ok := TokStr(v); ok {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n, true
		}
	}
	return 0, false
}

// FetchAttrs flattens a "* n FETCH (K V K V ...)" token list into a map
// keyed by upper-cased attribute name.
func FetchAttrs(tokens []any) map[string]any {
	out := map[string]any{}
	var list []any
	for _, t := range tokens {
		if l, ok := t.([]any); ok {
			list = l
			break
		}
	}
	if list == nil {
		list = tokens
	}
	i := 0
	for i+1 < len(list) {
		k, ok := TokStr(list[i])
		if !ok {
			i++
			continue
		}
		key := strings.ToUpper(k)
		if idx := strings.IndexByte(key, '['); idx > 0 {
			key = key[:idx] + "[]"
		}
		out[key] = list[i+1]
		i += 2
	}
	return out
}
