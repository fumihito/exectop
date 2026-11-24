package rpc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

const MaxMessage = 16 << 20

const streamWriteTimeout = 5 * time.Second

type Request struct {
	ID     int64           `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}
type Response struct {
	ID     int64     `json:"id"`
	Result any       `json:"result,omitempty"`
	Error  *RPCError `json:"error,omitempty"`
	Final  bool      `json:"final,omitempty"`
}
type RPCError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Reader keeps the buffered bytes belonging to one NDJSON connection. Use it
// for more than one response; constructing a new buffered reader per line can
// discard lines that arrived in the same network read.
type Reader struct{ br *bufio.Reader }

func NewReader(r io.Reader) *Reader { return &Reader{br: bufio.NewReader(r)} }
func (r *Reader) Read() (json.RawMessage, error) {
	return readNext(r.br)
}

func Read(r io.Reader) (json.RawMessage, error) {
	return readNext(bufio.NewReader(r))
}

func readNext(br *bufio.Reader) (json.RawMessage, error) {
	var line []byte
	for {
		part, e := br.ReadSlice('\n')
		line = append(line, part...)
		if len(line) > MaxMessage {
			return nil, fmt.Errorf("message too large")
		}
		if e == nil {
			break
		}
		if e != bufio.ErrBufferFull {
			if e == io.EOF && len(line) == 0 {
				return nil, io.EOF
			}
			return nil, e
		}
	}
	line = bytes.TrimSuffix(line, []byte{'\n'})
	line = bytes.TrimSuffix(line, []byte{'\r'})
	if !json.Valid(line) {
		return nil, fmt.Errorf("invalid JSON")
	}
	return append(json.RawMessage(nil), line...), nil
}
func DecodeRequest(b []byte) (Request, error) {
	var q Request
	if e := json.Unmarshal(b, &q); e != nil {
		return q, e
	}
	if q.ID < 0 || strings.TrimSpace(q.Method) == "" {
		return q, fmt.Errorf("invalid request")
	}
	return q, nil
}
func Write(w io.Writer, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	if len(b) > MaxMessage {
		return fmt.Errorf("message too large")
	}
	_, e = fmt.Fprintln(w, string(b))
	return e
}
func Serve(conn net.Conn, handler func(Request) []Response) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	greeted := false
	for {
		b, e := readNext(br)
		if e == io.EOF {
			return
		}
		if e != nil {
			_ = writeResponse(conn, Response{Error: &RPCError{Code: "invalid_request", Message: e.Error()}, Final: true})
			continue
		}
		q, e := DecodeRequest(b)
		if e != nil {
			_ = writeResponse(conn, Response{ID: q.ID, Error: &RPCError{Code: "invalid_request", Message: e.Error()}, Final: true})
			continue
		}
		if !greeted && q.Method != "hello" {
			_ = writeResponse(conn, Response{ID: q.ID, Error: &RPCError{Code: "hello_required", Message: "hello must be the first request"}, Final: true})
			continue
		}
		if q.Method == "hello" {
			greeted = true
		}
		for _, v := range handler(q) {
			if e = writeResponse(conn, v); e != nil {
				return
			}
		}
	}
}

// ServeStream is the subscription-capable server loop. A stream handler may
// return a channel after the initial response; closing the connection or a
// write failure ends the subscription and invokes cleanup.
func ServeStream(conn net.Conn, handler func(Request) []Response, stream func(Request) (<-chan Response, func(), bool)) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	greeted := false
	for {
		b, e := readNext(br)
		if e == io.EOF {
			return
		}
		if e != nil {
			_ = writeResponse(conn, Response{Error: &RPCError{Code: "invalid_request", Message: e.Error()}, Final: true})
			continue
		}
		q, e := DecodeRequest(b)
		if e != nil {
			_ = writeResponse(conn, Response{ID: q.ID, Error: &RPCError{Code: "invalid_request", Message: e.Error()}, Final: true})
			continue
		}
		if !greeted && q.Method != "hello" {
			_ = writeResponse(conn, Response{ID: q.ID, Error: &RPCError{Code: "hello_required", Message: "hello must be the first request"}, Final: true})
			continue
		}
		if q.Method == "hello" {
			greeted = true
		}
		if ch, cleanup, ok := stream(q); ok {
			// A run stream produces its own initial and final responses. Other
			// streams (currently subscribe) still use the regular handler for
			// their initial snapshot.
			if q.Method != "run" {
				for _, v := range handler(q) {
					if e = writeResponse(conn, v); e != nil {
						cleanup()
						return
					}
				}
			}
			clientClosed := make(chan struct{})
			go func() {
				var one [1]byte
				_, _ = conn.Read(one[:])
				close(clientClosed)
			}()
			for {
				select {
				case v, ok := <-ch:
					if !ok {
						cleanup()
						return
					}
					if e = writeResponse(conn, v); e != nil {
						cleanup()
						return
					}
				case <-clientClosed:
					cleanup()
					return
				}
			}
		}
		for _, v := range handler(q) {
			if e = writeResponse(conn, v); e != nil {
				return
			}
		}
	}
}

func writeResponse(conn net.Conn, v any) error {
	_ = conn.SetWriteDeadline(time.Now().Add(streamWriteTimeout))
	err := Write(conn, v)
	_ = conn.SetWriteDeadline(time.Time{})
	return err
}
