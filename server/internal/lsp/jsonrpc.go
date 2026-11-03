// Package lsp implements a minimal, dependency-free Language Server Protocol
// server on top of the engine package.
package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

type message struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *rpcError        `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type conn struct {
	r   *bufio.Reader
	w   io.Writer
	wmu sync.Mutex
}

func (c *conn) read() (*message, error) {
	length := -1
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return nil, fmt.Errorf("bad Content-Length: %v", err)
			}
			length = n
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("missing Content-Length")
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(c.r, buf); err != nil {
		return nil, err
	}
	var m message
	if err := json.Unmarshal(buf, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (c *conn) write(v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := fmt.Fprintf(c.w, "Content-Length: %d\r\n\r\n", len(b)); err != nil {
		return err
	}
	_, err = c.w.Write(b)
	return err
}

func (c *conn) reply(id *json.RawMessage, result interface{}, err *rpcError) error {
	out := map[string]interface{}{"jsonrpc": "2.0", "id": id}
	if err != nil {
		out["error"] = err
	} else {
		out["result"] = result
	}
	return c.write(out)
}

func (c *conn) notify(method string, params interface{}) error {
	return c.write(map[string]interface{}{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *conn) request(id int, method string, params interface{}) error {
	return c.write(map[string]interface{}{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
}

// ---------------------------------------------------------------------------
// URIs and positions
// ---------------------------------------------------------------------------

func uriToPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return ""
	}
	p := u.Path
	if runtime.GOOS == "windows" {
		p = strings.TrimPrefix(p, "/")
		if u.Host != "" {
			p = `\\` + u.Host + `\` + p // UNC
		}
	}
	return filepath.Clean(filepath.FromSlash(p))
}

func pathToURI(p string) string {
	p = filepath.ToSlash(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}

// byteToUTF16 converts a byte column in line to a UTF-16 code-unit column.
func byteToUTF16(line string, b int) int {
	if b > len(line) {
		b = len(line)
	}
	n := 0
	for i := 0; i < b; {
		r, size := utf8.DecodeRuneInString(line[i:])
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
		i += size
	}
	return n
}

// utf16ToByte converts a UTF-16 column to a byte column in line.
func utf16ToByte(line string, u int) int {
	n := 0
	for i := 0; i < len(line); {
		if n >= u {
			return i
		}
		r, size := utf8.DecodeRuneInString(line[i:])
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
		i += size
	}
	return len(line)
}

type position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type lspRange struct {
	Start position `json:"start"`
	End   position `json:"end"`
}

type textDocumentID struct {
	URI string `json:"uri"`
}

type textDocumentPositionParams struct {
	TextDocument textDocumentID `json:"textDocument"`
	Position     position       `json:"position"`
}
