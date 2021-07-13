//Copyright 2017 Improbable. All Rights Reserved.
// See LICENSE for licensing terms.

package grpcweb

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net/http"
	"strings"

	"golang.org/x/net/http2"
	"google.golang.org/grpc/grpclog"
)

// EnableGZip gzip switch
var EnableGZip = true

// grpcWebResponse implements http.ResponseWriter.
type grpcWebResponse struct {
	wroteHeaders bool
	wroteBody    bool
	headers      http.Header
	// Flush must be called on this writer before returning to ensure encoded buffer is flushed
	wrapped    http.ResponseWriter
	bodyWriter io.Writer
	body       *bytes.Buffer

	// The standard "application/grpc" content-type will be replaced with this.
	contentType string
}

func newGrpcWebResponse(resp http.ResponseWriter, isTextFormat bool) *grpcWebResponse {
	g := &grpcWebResponse{
		headers:     make(http.Header),
		wrapped:     resp,
		body:        &bytes.Buffer{},
		contentType: grpcWebContentType,
	}
	if isTextFormat {
		g.bodyWriter = newBase64ResponseWriter(g.body)
		g.contentType = grpcWebTextContentType
	}
	return g
}

func (w *grpcWebResponse) Header() http.Header {
	return w.headers
}

func (w *grpcWebResponse) Write(b []byte) (int, error) {
	if !w.wroteHeaders {
		w.prepareHeaders()
	}
	w.wroteBody, w.wroteHeaders = true, true
	return w.bodyWriter.Write(b)
}

func (w *grpcWebResponse) WriteHeader(code int) {
	w.prepareHeaders()
	w.wrapped.WriteHeader(code)
	w.wroteHeaders = true
}

func (w *grpcWebResponse) Flush() {
	if w.wroteHeaders || w.wroteBody {
		// Work around the fact that WriteHeader and a call to Flush would have caused a 200 response.
		// This is the case when there is no payload.
		w.wrapped.(http.Flusher).Flush()
	}
}

// prepareHeaders runs all required header copying and transformations to
// prepare the header of the wrapped response writer.
func (w *grpcWebResponse) prepareHeaders() {
	wh := w.wrapped.Header()
	copyHeader(
		wh, w.headers,
		skipKeys("trailer"),
		replaceInKeys(http2.TrailerPrefix, ""),
		replaceInVals("content-type", grpcContentType, w.contentType),
		keyCase(http.CanonicalHeaderKey),
	)
	responseHeaderKeys := headerKeys(wh)
	responseHeaderKeys = append(responseHeaderKeys, "grpc-status", "grpc-message")
	wh.Set(
		http.CanonicalHeaderKey("access-control-expose-headers"),
		strings.Join(responseHeaderKeys, ", "),
	)
	if EnableGZip {
		wh.Set(http.CanonicalHeaderKey("content-encoding"), "gzip")
	}
}

func (w *grpcWebResponse) finishRequest(req *http.Request) {
	if w.wroteHeaders || w.wroteBody {
		w.bodyWriter.(http.Flusher).Flush()
		io.Copy(w.wrapped, w.body)
		w.wrapped.(http.Flusher).Flush()
		w.copyTrailersToPayload()
	} else {
		w.WriteHeader(http.StatusOK)
		// https://github.com/zhlicen/grpc-web/issues/1
		// write error to trailer
		w.wrapped.Header().Del("grpc-status")
		w.wrapped.Header().Del("grpc-message")
		w.copyTrailersToPayload()
		// w.wrapped.(http.Flusher).Flush()
	}
}

func (w *grpcWebResponse) copyTrailersToPayload() {
	trailers := extractTrailingHeaders(w.headers, w.wrapped.Header())
	trailerBuffer := new(bytes.Buffer)
	trailers.Write(trailerBuffer)
	trailerGrpcDataHeader := []byte{1 << 7, 0, 0, 0, 0} // MSB=1 indicates this is a trailer data frame.
	binary.BigEndian.PutUint32(trailerGrpcDataHeader[1:5], uint32(trailerBuffer.Len()))
	w.wrapped.Write(trailerGrpcDataHeader)
	w.wrapped.Write(trailerBuffer.Bytes())
	w.wrapped.(http.Flusher).Flush()
}

func extractTrailingHeaders(src http.Header, flushed http.Header) http.Header {
	th := make(http.Header)
	copyHeader(
		th, src,
		skipKeys(append([]string{"trailer"}, headerKeys(flushed)...)...),
		replaceInKeys(http2.TrailerPrefix, ""),
		// gRPC-Web spec says that must use lower-case header/trailer names. See
		// "HTTP wire protocols" section in
		// https://github.com/grpc/grpc/blob/master/doc/PROTOCOL-WEB.md#protocol-differences-vs-grpc-over-http2
		keyCase(strings.ToLower),
	)
	return th
}

// An http.ResponseWriter wrapper that writes base64-encoded payloads. You must call Flush()
// on this writer to ensure the base64-encoder flushes its last state.
type base64ResponseWriter struct {
	wrapped io.Writer
	encoder io.WriteCloser
	gz      *GZWarpper
}

func newBase64ResponseWriter(wrapped io.Writer) io.Writer {
	w := &base64ResponseWriter{wrapped: wrapped}
	w.newEncoder()
	return w
}

// GZWarpper ...
type GZWarpper struct {
	gz      *gzip.Writer
	wrapped io.Writer
}

func (gw *GZWarpper) Write(p []byte) (n int, err error) {
	return gw.gz.Write(p)
}

// Flush ...
func (gw *GZWarpper) Flush() {
	gw.gz.Close()
}

// NewGZWarpper ...
func NewGZWarpper(w io.Writer) *GZWarpper {
	gz, _ := gzip.NewWriterLevel(w, gzip.BestCompression)
	return &GZWarpper{gz: gz, wrapped: w}
}

func (w *base64ResponseWriter) newEncoder() {
	if EnableGZip {
		gz := NewGZWarpper(w.wrapped)
		w.gz = gz
		w.encoder = base64.NewEncoder(base64.StdEncoding, gz)
	} else {
		w.encoder = base64.NewEncoder(base64.StdEncoding, w.wrapped)
	}
}

// func (w *base64ResponseWriter) Header() http.Header {
// 	return w.wrapped.Header()
// }

func (w *base64ResponseWriter) Write(b []byte) (int, error) {
	return w.encoder.Write(b)
}

// func (w *base64ResponseWriter) WriteHeader(code int) {
// 	w.wrapped.WriteHeader(code)
// }

func (w *base64ResponseWriter) Flush() {
	// Flush the base64 encoder by closing it. Grpc-web permits multiple padded base64 parts:
	// https://github.com/grpc/grpc/blob/master/doc/PROTOCOL-WEB.md
	err := w.encoder.Close()
	if err != nil {
		// Must ignore this error since Flush() is not defined as returning an error
		grpclog.Errorf("ignoring error Flushing base64 encoder: %v", err)
	}
	w.gz.Flush()
	w.newEncoder()
	// w.wrapped.(io.Closer).Close()
}
