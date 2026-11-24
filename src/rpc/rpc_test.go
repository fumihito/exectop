package rpc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"testing"
	"time"
)

func TestNDJSON(t *testing.T) {
	b, e := Read(bytes.NewBufferString("{\"id\":1,\"method\":\"status\"}\n"))
	if e != nil {
		t.Fatal(e)
	}
	q, e := DecodeRequest(b)
	if e != nil || q.Method != "status" {
		t.Fatal(q, e)
	}
}

func TestReadNextPreservesCoalescedRequests(t *testing.T) {
	br := bufio.NewReader(bytes.NewBufferString("{\"id\":1,\"method\":\"status\"}\n{\"id\":2,\"method\":\"list\"}\n"))
	for _, want := range []string{"status", "list"} {
		b, e := readNext(br)
		if e != nil {
			t.Fatal(e)
		}
		q, e := DecodeRequest(b)
		if e != nil || q.Method != want {
			t.Fatalf("method=%q err=%v", q.Method, e)
		}
	}
}

func TestServeStreamRunSkipsUnaryHandlerAndWritesFinal(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	ch := make(chan Response, 2)
	ch <- Response{ID: 7, Result: map[string]string{"item": "one"}}
	ch <- Response{ID: 7, Final: true}
	close(ch)
	done := make(chan struct{})
	go func() {
		ServeStream(server, func(q Request) []Response {
			if q.Method == "hello" {
				return []Response{{ID: q.ID, Result: "hello"}}
			}
			t.Errorf("unary handler was called for run stream")
			return nil
		}, func(q Request) (<-chan Response, func(), bool) {
			if q.Method == "hello" {
				return nil, nil, false
			}
			return ch, func() {}, true
		})
		close(done)
	}()
	if e := Write(client, Request{ID: 0, Method: "hello"}); e != nil {
		t.Fatal(e)
	}
	if _, e := Read(client); e != nil {
		t.Fatal(e)
	}
	if e := Write(client, Request{ID: 7, Method: "run"}); e != nil {
		t.Fatal(e)
	}
	first, e := Read(client)
	if e != nil || first == nil {
		t.Fatal(first, e)
	}
	second, e := Read(client)
	if e != nil || second == nil {
		t.Fatal(second, e)
	}
	var final Response
	if e = json.Unmarshal(second, &final); e != nil || !final.Final {
		t.Fatalf("final=%+v err=%v", final, e)
	}
	<-done
}

func TestServeStreamCleansUpWhenSubscriberDisconnects(t *testing.T) {
	server, client := net.Pipe()
	ch := make(chan Response)
	cleaned := make(chan struct{})
	done := make(chan struct{})
	go func() {
		ServeStream(server, func(q Request) []Response {
			if q.Method == "hello" {
				return []Response{{ID: q.ID, Result: "hello"}}
			}
			return []Response{{ID: q.ID, Result: []any{}, Final: false}}
		}, func(q Request) (<-chan Response, func(), bool) {
			if q.Method == "hello" {
				return nil, nil, false
			}
			return ch, func() { close(cleaned) }, true
		})
		close(done)
	}()
	if e := Write(client, Request{ID: 0, Method: "hello"}); e != nil {
		t.Fatal(e)
	}
	if _, e := Read(client); e != nil {
		t.Fatal(e)
	}
	if e := Write(client, Request{ID: 1, Method: "subscribe"}); e != nil {
		t.Fatal(e)
	}
	if _, e := Read(client); e != nil {
		t.Fatal(e)
	}
	_ = client.Close()
	select {
	case <-cleaned:
	case <-time.After(time.Second):
		t.Fatal("subscription cleanup did not run")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream server did not exit")
	}
}
