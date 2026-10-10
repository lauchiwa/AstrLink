package transport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/astrlink/core/internal/networkproxy"
	"github.com/gorilla/websocket"
)

// ResponsesSocket owns a single upstream connection. Turns are serial, while
// Close and cancellation may run concurrently. The HTTP-shaped stream allows
// existing privacy restoration, usage scanning and audit capture to stay shared.
type ResponsesSocket struct {
	mu       sync.Mutex
	writeMu  sync.Mutex
	conn     *websocket.Conn
	closed   bool
	binding  string
	messages chan responsesSocketMessage
	done     chan struct{}
}

func (socket *ResponsesSocket) Connected() bool {
	socket.mu.Lock()
	defer socket.mu.Unlock()
	return socket.conn != nil
}

func (socket *ResponsesSocket) Close() {
	socket.mu.Lock()
	defer socket.mu.Unlock()
	if !socket.closed && socket.done != nil {
		close(socket.done)
	}
	socket.closed = true
	if socket.conn != nil {
		_ = socket.conn.Close()
	}
}

func (socket *ResponsesSocket) write(conn *websocket.Conn, data []byte) error {
	socket.writeMu.Lock()
	defer socket.writeMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return conn.WriteMessage(websocket.TextMessage, data)
}

// handshakeDigest fingerprints the headers a turn would hand to the WebSocket
// handshake. It hashes rather than retains them, because the effective set
// includes the upstream credential.
//
// Names are canonicalized and sorted, and values keep their order within a
// name, so two turns that would dial identically produce one digest regardless
// of map iteration order. Lengths are encoded so no pair of adjacent fields can
// be rearranged into the same byte stream.
func handshakeDigest(header http.Header) string {
	names := make([]string, 0, len(header))
	for name := range header {
		names = append(names, name)
	}
	sort.Strings(names)
	digest := sha256.New()
	for _, name := range names {
		canonical := http.CanonicalHeaderKey(name)
		fmt.Fprintf(digest, "%d:%s", len(canonical), canonical)
		for _, value := range header[name] {
			fmt.Fprintf(digest, "%d:%s", len(value), value)
		}
	}
	return fmt.Sprintf("%x", digest.Sum(nil))
}

// Forward emits exactly one response.create. Once sent, failures are response
// errors and must never replay the turn on another channel or connection.
func (socket *ResponsesSocket) Forward(writer http.ResponseWriter, request *http.Request, target Target, binding string, controls <-chan []byte) error {
	if err := validateTarget(target); err != nil {
		return &TargetError{err: err}
	}
	ctx, err := networkproxy.Bind(request.Context(), target.Service, target.ProxyCredentials)
	if err != nil {
		return &TargetError{err: err}
	}
	binding += ":" + networkproxy.Binding(ctx)
	outbound := request.Clone(ctx)
	outbound.URL = joinTargetURL(target.BaseURL, request.URL)
	outbound.Header = request.Header.Clone()
	removeHopByHopHeaders(outbound.Header)
	removeInboundCredentials(outbound.Header)
	overlayHeaders(outbound.Header, target.RequestHeaders)
	removeHopByHopHeaders(outbound.Header)
	removeGatewayHeaders(outbound.Header)
	removeForwardingHeaders(outbound.Header)
	for name := range outbound.Header {
		if strings.HasPrefix(strings.ToLower(name), "sec-websocket-") {
			outbound.Header.Del(name)
		}
	}
	for _, name := range []string{"Content-Type", "Content-Length", "Accept-Encoding", "Accept"} {
		outbound.Header.Del(name)
	}
	// The channel identity covers the headers this turn would actually hand to
	// the handshake, computed after every removal above. Digesting the caller's
	// prepared configuration instead would be wrong in both directions: a value
	// stripped before dialing would force a pointless reconnect, and a value the
	// caller never prepared but the request carries would reuse a channel it does
	// not belong to.
	binding += ":" + handshakeDigest(outbound.Header)
	socket.mu.Lock()
	conn, closed, previousBinding := socket.conn, socket.closed, socket.binding
	socket.mu.Unlock()
	// Admission is decided before the turn is reported as dispatched. A refused
	// turn never reaches this provider, so observing it would open an attempt
	// record and arm identity capture for a request that was never sent.
	if closed {
		return NewResponseError(errors.New("upstream WebSocket is closed"))
	}
	if conn != nil && previousBinding != binding {
		return NewResponseError(errors.New("upstream WebSocket binding changed; reconnect to use a different channel or credential"))
	}
	if target.ObserveOutbound != nil {
		// Runs before the body is read so an observer can tee it.
		target.ObserveOutbound(outbound)
	}
	body, err := io.ReadAll(outbound.Body)
	if err != nil {
		return NewUpstreamError(err)
	}
	var event map[string]json.RawMessage
	if err := json.Unmarshal(body, &event); err != nil || event == nil {
		return &TargetError{err: errors.New("invalid response.create")}
	}
	event["type"] = json.RawMessage(`"response.create"`)
	for _, key := range []string{"stream", "stream_options", "background"} {
		delete(event, key)
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return &TargetError{err: err}
	}
	if conn == nil {
		dialer := *websocket.DefaultDialer
		if err := networkproxy.ConfigureWebSocket(ctx, &dialer, outbound.URL); err != nil {
			return NewUpstreamError(err)
		}
		url := *outbound.URL
		if url.Scheme == "https" {
			url.Scheme = "wss"
		} else {
			url.Scheme = "ws"
		}
		var response *http.Response
		conn, response, err = dialer.DialContext(ctx, url.String(), outbound.Header)
		if err != nil {
			if response != nil && response.Body != nil {
				if target.WrapResponseBody != nil {
					response.Body = target.WrapResponseBody(response.StatusCode, response.Header, response.Body)
				}
				if target.HandleResponse != nil {
					return target.HandleResponse(response)
				}
				return WriteResponse(writer, response)
			}
			return NewUpstreamError(err)
		}
		conn.SetReadLimit(128 << 20)
		socket.mu.Lock()
		if socket.closed {
			socket.mu.Unlock()
			_ = conn.Close()
			return NewResponseError(context.Canceled)
		}
		socket.conn, socket.binding = conn, binding
		socket.messages = make(chan responsesSocketMessage)
		socket.done = make(chan struct{})
		socket.mu.Unlock()
		go socket.readMessages(conn)
	}
	stopClose := context.AfterFunc(request.Context(), socket.Close)
	defer stopClose()
	if err := socket.write(conn, payload); err != nil {
		socket.Close()
		return NewResponseError(err)
	}
	controlCtx, cancelControls := context.WithCancel(request.Context())
	controlsDone := make(chan struct{})
	go func() {
		defer close(controlsDone)
		select {
		case control := <-controls:
			if err := socket.write(conn, control); err != nil {
				socket.Close()
			}
		case <-controlCtx.Done():
		}
	}()
	defer func() { cancelControls(); <-controlsDone }()
	reader := &responsesEventReader{socket: socket}
	defer reader.Close()
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: reader}
	if target.WrapResponseBody != nil {
		response.Body = target.WrapResponseBody(response.StatusCode, response.Header, reader)
	}
	if target.HandleResponse != nil {
		return target.HandleResponse(response)
	}
	return WriteResponse(writer, response)
}

// Each WebSocket message is one Responses event; SSE is an internal envelope
// only, consumed by the same writers as HTTP Responses.
type responsesEventReader struct {
	socket   *ResponsesSocket
	pending  *bytes.Reader
	terminal bool
	failed   bool
}

func (reader *responsesEventReader) Read(buffer []byte) (int, error) {
	for reader.pending == nil || reader.pending.Len() == 0 {
		if reader.terminal {
			if reader.failed {
				return 0, errors.New("upstream Responses generation did not complete successfully")
			}
			return 0, io.EOF
		}
		var message responsesSocketMessage
		select {
		case message = <-reader.socket.messages:
		case <-reader.socket.done:
			return 0, io.ErrClosedPipe
		}
		kind, data, err := message.kind, message.data, message.err
		if err != nil {
			return 0, err
		}
		var event struct {
			Type string `json:"type"`
		}
		if kind != websocket.TextMessage || json.Unmarshal(data, &event) != nil || event.Type == "" {
			return 0, errors.New("invalid upstream Responses WebSocket event")
		}
		reader.terminal = ResponsesTerminalEvent(event.Type)
		reader.failed = reader.terminal && event.Type != "response.completed" && event.Type != "response.done"
		// Compact JSON prevents embedded pretty-print newlines breaking SSE framing.
		var compact bytes.Buffer
		if err := json.Compact(&compact, data); err != nil {
			return 0, err
		}
		reader.pending = bytes.NewReader(append(append([]byte("data: "), compact.Bytes()...), '\n', '\n'))
	}
	return reader.pending.Read(buffer)
}
func (reader *responsesEventReader) Close() error {
	if !reader.terminal {
		reader.socket.Close()
	}
	return nil
}
func ResponsesTerminalEvent(kind string) bool {
	switch kind {
	case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "error":
		return true
	default:
		return false
	}
}

// Keep reading between turns so upstream ping/close frames are processed.
// The unbuffered channel bounds queued payloads to one event.
type responsesSocketMessage struct {
	kind int
	data []byte
	err  error
}

func (socket *ResponsesSocket) readMessages(conn *websocket.Conn) {
	for {
		kind, data, err := conn.ReadMessage()
		select {
		case socket.messages <- responsesSocketMessage{kind, data, err}:
		case <-socket.done:
			return
		}
		if err != nil {
			return
		}
	}
}
