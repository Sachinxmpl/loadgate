package proxy

import (
	"bytes"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	type res struct {
		c   net.Conn
		err error
	}

	ch := make(chan res, 1)

	// exists when accept retruns
	go func() {
		c, err := ln.Accept()
		ch <- res{c, err}
	}()

	dialed, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	got := <-ch
	if got.err != nil {
		t.Fatalf("accept: %v", got.err)
	}
	return dialed, got.c
}

// truncation test
// client sends a request, half-closes and then waits for a reply the backend sends onlyafter seeing that half close.
// A proxy that full-closes on the first EOF would drop the reply

func TestL4_HalfCloseDeliverReply(t *testing.T) {
	clientProxy, clientTest := tcpPair(t)
	backendProxy, backendTest := tcpPair(t)

	// exists when: both copy directions finish
	go L4(clientProxy, backendProxy, time.Second)

	const request = "GET please\n"
	const reply = "here is your answer\n"

	// fake backend
	// read whole request until client's half-close gives us EOF, then send reply, then close

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		got, err := io.ReadAll(backendTest)
		if err != nil {
			t.Errorf("backend read: %v", err)
			return
		}
		if string(got) != request {
			t.Errorf("backend got %q, want %q", got, request)
		}
		backendTest.Write([]byte(reply))
		backendTest.Close()
	}()

	if _, err := clientTest.Write([]byte(request)); err != nil {
		t.Fatalf("client writes: %v", err)
	}

	clientTest.(*net.TCPConn).CloseWrite()

	clientTest.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := io.ReadAll(clientTest)

	if err != nil {
		t.Fatalf("Client read reply: %v", err)
	}
	if !bytes.Equal(got, []byte(reply)) {
		t.Errorf("client got reply %q, want %q", got, reply)
	}
	wg.Wait()
}

// Laggard Test
// backend goes slient after clienf half_closes. Drain deadline must fire and let pipe finish, leaking no goroutine

func TestL4_SlientBackendHitsDrainDeadline(t *testing.T) {
	clientProxy, clientTest := tcpPair(t)
	backendProxy, backendTest := tcpPair(t)

	// fake backend that never replies
	defer backendTest.Close()

	done := make(chan struct{})

	//exists when: L4 returns , which the drain deadline guarantees is bounded
	go func() {
		L4(clientProxy, backendProxy, 200*time.Millisecond)
		close(done)
	}()

	//  client sends, then half-closes, then backend never answers
	clientTest.Write([]byte("anyone there? \n"))
	clientTest.(*net.TCPConn).CloseWrite()

	select {
	case <-done:
		// L4 returned on its own within the deadline correct
	case <-time.After(2 * time.Second):
		t.Fatal("L4 didnot return; drain deadline never fired")
	}

	clientTest.Close()
}

// Slow-reader test
func TestL4_NonReadingClientHitsDrainDeadline(t *testing.T) {
	clientProxy, clientTest := tcpPair(t)
	backendProxy, backendTest := tcpPair(t)
	defer clientTest.Close()

	done := make(chan struct{})
	go func() {
		L4(clientProxy, backendProxy, 200*time.Millisecond)
		close(done)
	}()

	// fake backend: floods the client with 64MB, which it never reads
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer backendTest.Close()
		backendTest.Write(make([]byte, 64<<20))
	}()

	clientTest.Write([]byte("send me everything\n"))
	clientTest.(*net.TCPConn).CloseWrite()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("L4 did not return: blocked writing to a client that never reads")
	}
	wg.Wait()
}
