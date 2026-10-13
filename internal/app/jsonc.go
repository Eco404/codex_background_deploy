package app

import (
	"encoding/json"
	"errors"
	"strings"
)

// Keep byte offsets while blanking comments; strip trailing commas outside strings.
func cleanJSONC(b []byte) ([]byte, error) {
	o := append([]byte{}, b...)
	in, escape := false, false
	for i := 0; i < len(o); i++ {
		c := o[i]
		if in {
			if escape {
				escape = false
			} else if c == '\\' {
				escape = true
			} else if c == '"' {
				in = false
			}
			continue
		}
		if c == '"' {
			in = true
			continue
		}
		if c == '/' && i+1 < len(o) && o[i+1] == '/' {
			o[i] = ' '
			i++
			for i < len(o) && o[i] != '\n' {
				o[i] = ' '
				i++
			}
			continue
		}
		if c == '/' && i+1 < len(o) && o[i+1] == '*' {
			o[i] = ' '
			i++
			o[i] = ' '
			closed := false
			for i++; i < len(o); i++ {
				if o[i] == '*' && i+1 < len(o) && o[i+1] == '/' {
					o[i] = ' '
					i++
					o[i] = ' '
					closed = true
					break
				}
				if o[i] != '\n' {
					o[i] = ' '
				}
			}
			if !closed {
				return nil, errors.New("JSONC注释未闭合")
			}
		}
	}
	in = false
	escape = false
	for i, c := range o {
		if in {
			if escape {
				escape = false
			} else if c == '\\' {
				escape = true
			} else if c == '"' {
				in = false
			}
			continue
		}
		if c == '"' {
			in = true
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(o) && strings.ContainsRune(" \r\n\t", rune(o[j])) {
				j++
			}
			if j < len(o) && (o[j] == '}' || o[j] == ']') {
				o[i] = ' '
			}
		}
	}
	if !json.Valid(o) {
		return nil, errors.New("JSONC无效")
	}
	return o, nil
}

type jsonMember struct {
	key                    string
	start, end, valueStart int
	value                  string
}

func jsonMembers(b []byte) ([]jsonMember, int, error) {
	clean, e := cleanJSONC(b)
	if e != nil {
		return nil, 0, e
	}
	i := 0
	space := func() {
		for i < len(clean) && strings.ContainsRune(" \r\n\t", rune(clean[i])) {
			i++
		}
	}
	space()
	if i >= len(clean) || clean[i] != '{' {
		return nil, 0, errors.New("配置不是对象")
	}
	i++
	out := []jsonMember{}
	seen := map[string]bool{}
	for {
		space()
		if clean[i] == '}' {
			return out, i, nil
		}
		start := i
		d := json.NewDecoder(strings.NewReader(string(clean[i:])))
		var key string
		if e = d.Decode(&key); e != nil {
			return nil, 0, e
		}
		if seen[key] {
			return nil, 0, errors.New("JSONC重复键")
		}
		seen[key] = true
		i += int(d.InputOffset())
		space()
		if clean[i] != ':' {
			return nil, 0, errors.New("缺少冒号")
		}
		i++
		space()
		vs := i
		d = json.NewDecoder(strings.NewReader(string(clean[i:])))
		var raw json.RawMessage
		if e = d.Decode(&raw); e != nil {
			return nil, 0, e
		}
		i += int(d.InputOffset())
		var val string
		_ = json.Unmarshal(raw, &val)
		out = append(out, jsonMember{key, start, i, vs, val})
		space()
		if clean[i] == ',' {
			i++
			continue
		}
		if clean[i] == '}' {
			return out, i, nil
		}
		return nil, 0, errors.New("配置成员分隔符无效")
	}
}
func configValue(b []byte, key string) (string, bool, error) {
	members, _, e := jsonMembers(b)
	if e != nil {
		return "", false, e
	}
	for _, m := range members {
		if m.key == key {
			return m.value, true, nil
		}
	}
	return "", false, nil
}
func editConfigKey(b []byte, key string, value *string) ([]byte, error) {
	members, close, e := jsonMembers(b)
	if e != nil {
		return nil, e
	}
	for n, m := range members {
		if m.key != key {
			continue
		}
		if value != nil {
			v, _ := json.Marshal(*value)
			out := append([]byte{}, b[:m.valueStart]...)
			out = append(out, v...)
			return append(out, b[m.end:]...), nil
		}
		start, end := m.start, m.end
		if n < len(members)-1 {
			end = members[n+1].start
		} else if n > 0 {
			start = members[n-1].end
		} else {
			end = close
		}
		out := append([]byte{}, b[:start]...)
		return append(out, b[end:]...), nil
	}
	if value == nil {
		return b, nil
	}
	v, _ := json.Marshal(*value)
	k, _ := json.Marshal(key)
	insert := append(k, []byte(": ")...)
	insert = append(insert, v...)
	if len(members) > 0 {
		last := members[len(members)-1]
		out := append([]byte{}, b[:last.end]...)
		out = append(out, ',')
		out = append(out, insert...)
		return append(out, b[last.end:]...), nil
	}
	out := append([]byte{}, b[:close]...)
	out = append(out, insert...)
	return append(out, b[close:]...), nil
}
