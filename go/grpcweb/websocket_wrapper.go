package grpcweb

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/gorilla/websocket"
	"golang.org/x/net/http2"
)

type webSocketResponseWriter struct {
	writtenHeaders  bool
	wsConn          *websocket.Conn
	headers         http.Header
	flushedHeaders  http.Header
	closeNotifyChan chan bool
}

func newWebSocketResponseWriter(wsConn *websocket.Conn) *webSocketResponseWriter {
	closeNotifyChan := make(chan bool)
	wsConn.SetCloseHandler(func(code int, text string) error {
		close(closeNotifyChan)
		return nil
	})

	return &webSocketResponseWriter{
		writtenHeaders:  false,
		headers:         make(http.Header),
		flushedHeaders:  make(http.Header),
		wsConn:          wsConn,
		closeNotifyChan: closeNotifyChan,
	}
}

func (w *webSocketResponseWriter) Header() http.Header {
	return w.headers
}

func (w *webSocketResponseWriter) Write(b []byte) (int, error) {
	if !w.writtenHeaders {
		w.WriteHeader(http.StatusOK)
	}
	return len(b), w.wsConn.WriteMessage(websocket.BinaryMessage, b)
}

func (w *webSocketResponseWriter) writeHeaderFrame(headers http.Header) {
	headerBuffer := new(bytes.Buffer)
	headers.Write(headerBuffer)
	headerGrpcDataHeader := []byte{1 << 7, 0, 0, 0, 0} // MSB=1 indicates this is a header data frame.
	binary.BigEndian.PutUint32(headerGrpcDataHeader[1:5], uint32(headerBuffer.Len()))
	w.wsConn.WriteMessage(websocket.BinaryMessage, headerGrpcDataHeader)
	w.wsConn.WriteMessage(websocket.BinaryMessage, headerBuffer.Bytes())
}

func (w *webSocketResponseWriter) copyFlushedHeaders() {
	for k, vv := range w.headers {
		// Skip the pre-annoucement of Trailer headers. Don't add them to the response headers.
		if strings.ToLower(k) == "trailer" {
			continue
		}
		for _, v := range vv {
			w.flushedHeaders.Add(k, v)
		}
	}
}

func (w *webSocketResponseWriter) WriteHeader(code int) {
	w.copyFlushedHeaders()
	w.writtenHeaders = true
	w.writeHeaderFrame(w.headers)
	return
}

func (w *webSocketResponseWriter) extractTrailerHeaders() http.Header {
	trailerHeaders := make(http.Header)
	for k, vv := range w.headers {
		// Skip the pre-annoucement of Trailer headers. Don't add them to the response headers.
		if strings.ToLower(k) == "trailer" {
			continue
		}
		// Skip existing headers that were already sent.
		if _, exists := w.flushedHeaders[k]; exists {
			continue
		}
		// Skip the Trailer prefix
		if strings.HasPrefix(k, http2.TrailerPrefix) {
			k = k[len(http2.TrailerPrefix):]
		}
		for _, v := range vv {
			trailerHeaders.Add(k, v)
		}
	}
	return trailerHeaders
}

func (w *webSocketResponseWriter) FlushTrailers() {
	w.writeHeaderFrame(w.extractTrailerHeaders())
}

func (w *webSocketResponseWriter) Flush() {
	// no-op
}

func (w *webSocketResponseWriter) CloseNotify() <-chan bool {
	return w.closeNotifyChan
}

type webSocketWrappedReader struct {
	wsConn     *websocket.Conn
	respWriter *webSocketResponseWriter
	reader     *io.PipeReader
}

func (w *webSocketWrappedReader) Close() error {
	w.respWriter.FlushTrailers()
	return w.wsConn.Close()
}

// First byte of a binary WebSocket frame is used for control flow:
// 0 = Data
// 1 = End of client send
func (w *webSocketWrappedReader) Read(p []byte) (int, error) {
	return w.reader.Read(p)
}

func newWebsocketWrappedReader(wsConn *websocket.Conn, respWriter *webSocketResponseWriter, cancel context.CancelFunc) *webSocketWrappedReader {
	pipeRead, pipeWrite := io.Pipe()

	go func() {
		for {
			// Read a whole frame from the WebSocket connection
			messageType, framePayload, err := wsConn.ReadMessage()
			if err == io.EOF || messageType == -1 {
				// The client has closed the connection. Indicate to the response writer that it should close
				cancel()
				pipeWrite.CloseWithError(io.EOF)
				return
			}

			// Only Binary frames are valid
			if messageType != websocket.BinaryMessage {
				continue
			}

			// The first byte is used for control flow, so the data starts from the second byte
			pipeWrite.Write(framePayload[1:])
		}
	}()

	return &webSocketWrappedReader{
		wsConn:     wsConn,
		respWriter: respWriter,
		reader:     pipeRead,
	}
}

func parseHeaders(headerString string) (http.Header, error) {
	reader := bufio.NewReader(strings.NewReader(headerString + "\r\n"))
	tp := textproto.NewReader(reader)

	mimeHeader, err := tp.ReadMIMEHeader()
	if err != nil {
		return nil, err
	}

	// http.Header and textproto.MIMEHeader are both just a map[string][]string
	return http.Header(mimeHeader), nil
}
