package prom_mux

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	flusher = 1 << iota
	hijacker
	readerFrom
	pusher
)

type delegator interface {
	http.ResponseWriter

	Status() int
	Written() int64
}

type responseWriterDelegator struct {
	http.ResponseWriter

	status             int
	written            int64
	wroteHeader        bool
	observeWriteHeader func(int)
}

func (r *responseWriterDelegator) Status() int {
	return r.status
}

func (r *responseWriterDelegator) Written() int64 {
	return r.written
}

func (r *responseWriterDelegator) WriteHeader(code int) {
	if r.observeWriteHeader != nil && !r.wroteHeader {
		// Only call observeWriteHeader for the 1st time. It's a bug if
		// WriteHeader is called more than once, but we want to protect
		// against it here. Note that we still delegate the WriteHeader
		// to the original ResponseWriter to not mask the bug from it.
		r.observeWriteHeader(code)
	}
	r.status = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseWriterDelegator) Write(b []byte) (int, error) {
	// If applicable, call WriteHeader here so that observeWriteHeader is
	// handled appropriately.
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}

type flusherDelegator struct{ *responseWriterDelegator }
type hijackerDelegator struct{ *responseWriterDelegator }
type readerFromDelegator struct{ *responseWriterDelegator }
type pusherDelegator struct{ *responseWriterDelegator }

func (d flusherDelegator) Flush() {
	// If applicable, call WriteHeader here so that observeWriteHeader is
	// handled appropriately.
	if !d.wroteHeader {
		d.WriteHeader(http.StatusOK)
	}
	d.ResponseWriter.(http.Flusher).Flush()
}
func (d hijackerDelegator) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return d.ResponseWriter.(http.Hijacker).Hijack()
}
func (d readerFromDelegator) ReadFrom(re io.Reader) (int64, error) {
	// If applicable, call WriteHeader here so that observeWriteHeader is
	// handled appropriately.
	if !d.wroteHeader {
		d.WriteHeader(http.StatusOK)
	}
	n, err := d.ResponseWriter.(io.ReaderFrom).ReadFrom(re)
	d.written += n
	return n, err
}
func (d pusherDelegator) Push(target string, opts *http.PushOptions) error {
	return d.ResponseWriter.(http.Pusher).Push(target, opts)
}

var pickDelegator = make([]func(*responseWriterDelegator) delegator, 32)

func init() {
	// TODO(beorn7): Code generation would help here.
	pickDelegator[0] = func(d *responseWriterDelegator) delegator {
		return d
	}
	pickDelegator[flusher] = func(d *responseWriterDelegator) delegator {
		return flusherDelegator{d}
	}
	pickDelegator[hijacker] = func(d *responseWriterDelegator) delegator {
		return hijackerDelegator{d}
	}
	pickDelegator[hijacker+flusher] = func(d *responseWriterDelegator) delegator {
		return struct {
			*responseWriterDelegator
			http.Hijacker
			http.Flusher
		}{d, hijackerDelegator{d}, flusherDelegator{d}}
	}
	pickDelegator[readerFrom] = func(d *responseWriterDelegator) delegator {
		return readerFromDelegator{d}
	}
	pickDelegator[readerFrom+flusher] = func(d *responseWriterDelegator) delegator { // 10
		return struct {
			*responseWriterDelegator
			io.ReaderFrom
			http.Flusher
		}{d, readerFromDelegator{d}, flusherDelegator{d}}
	}
	pickDelegator[readerFrom+hijacker] = func(d *responseWriterDelegator) delegator { // 12
		return struct {
			*responseWriterDelegator
			io.ReaderFrom
			http.Hijacker
		}{d, readerFromDelegator{d}, hijackerDelegator{d}}
	}
	pickDelegator[readerFrom+hijacker+flusher] = func(d *responseWriterDelegator) delegator { // 14
		return struct {
			*responseWriterDelegator
			io.ReaderFrom
			http.Hijacker
			http.Flusher
		}{d, readerFromDelegator{d}, hijackerDelegator{d}, flusherDelegator{d}}
	}
	pickDelegator[pusher] = func(d *responseWriterDelegator) delegator { // 16
		return pusherDelegator{d}
	}
	pickDelegator[pusher+flusher] = func(d *responseWriterDelegator) delegator { // 18
		return struct {
			*responseWriterDelegator
			http.Pusher
			http.Flusher
		}{d, pusherDelegator{d}, flusherDelegator{d}}
	}
	pickDelegator[pusher+hijacker] = func(d *responseWriterDelegator) delegator { // 20
		return struct {
			*responseWriterDelegator
			http.Pusher
			http.Hijacker
		}{d, pusherDelegator{d}, hijackerDelegator{d}}
	}
	pickDelegator[pusher+hijacker+flusher] = func(d *responseWriterDelegator) delegator { // 22
		return struct {
			*responseWriterDelegator
			http.Pusher
			http.Hijacker
			http.Flusher
		}{d, pusherDelegator{d}, hijackerDelegator{d}, flusherDelegator{d}}
	}
	pickDelegator[pusher+readerFrom] = func(d *responseWriterDelegator) delegator { // 24
		return struct {
			*responseWriterDelegator
			http.Pusher
			io.ReaderFrom
		}{d, pusherDelegator{d}, readerFromDelegator{d}}
	}
	pickDelegator[pusher+readerFrom+flusher] = func(d *responseWriterDelegator) delegator { // 26
		return struct {
			*responseWriterDelegator
			http.Pusher
			io.ReaderFrom
			http.Flusher
		}{d, pusherDelegator{d}, readerFromDelegator{d}, flusherDelegator{d}}
	}
	pickDelegator[pusher+readerFrom+hijacker] = func(d *responseWriterDelegator) delegator { // 28
		return struct {
			*responseWriterDelegator
			http.Pusher
			io.ReaderFrom
			http.Hijacker
		}{d, pusherDelegator{d}, readerFromDelegator{d}, hijackerDelegator{d}}
	}
	pickDelegator[pusher+readerFrom+hijacker+flusher] = func(d *responseWriterDelegator) delegator { // 30
		return struct {
			*responseWriterDelegator
			http.Pusher
			io.ReaderFrom
			http.Hijacker
			http.Flusher
		}{d, pusherDelegator{d}, readerFromDelegator{d}, hijackerDelegator{d}, flusherDelegator{d}}
	}
}

func newDelegator(w http.ResponseWriter, observeWriteHeaderFunc func(int)) delegator {
	d := &responseWriterDelegator{
		ResponseWriter:     w,
		observeWriteHeader: observeWriteHeaderFunc,
	}

	id := 0
	if _, ok := w.(http.Flusher); ok {
		id += flusher
	}
	if _, ok := w.(http.Hijacker); ok {
		id += hijacker
	}
	if _, ok := w.(io.ReaderFrom); ok {
		id += readerFrom
	}
	if _, ok := w.(http.Pusher); ok {
		id += pusher
	}

	return pickDelegator[id](d)
}

func sanitizeMethod(m string) string {
	switch m {
	case "GET", "get":
		return "get"
	case "PUT", "put":
		return "put"
	case "HEAD", "head":
		return "head"
	case "POST", "post":
		return "post"
	case "DELETE", "delete":
		return "delete"
	case "CONNECT", "connect":
		return "connect"
	case "OPTIONS", "options":
		return "options"
	case "NOTIFY", "notify":
		return "notify"
	default:
		return strings.ToLower(m)
	}
}

// If the wrapped http.Handler has not set a status code, i.e. the value is
// currently 0, santizeCode will return 200, for consistency with behavior in
// the stdlib.
func sanitizeCode(s int) string {
	switch s {
	case 100:
		return "100"
	case 101:
		return "101"

	case 200, 0:
		return "200"
	case 201:
		return "201"
	case 202:
		return "202"
	case 203:
		return "203"
	case 204:
		return "204"
	case 205:
		return "205"
	case 206:
		return "206"

	case 300:
		return "300"
	case 301:
		return "301"
	case 302:
		return "302"
	case 304:
		return "304"
	case 305:
		return "305"
	case 307:
		return "307"

	case 400:
		return "400"
	case 401:
		return "401"
	case 402:
		return "402"
	case 403:
		return "403"
	case 404:
		return "404"
	case 405:
		return "405"
	case 406:
		return "406"
	case 407:
		return "407"
	case 408:
		return "408"
	case 409:
		return "409"
	case 410:
		return "410"
	case 411:
		return "411"
	case 412:
		return "412"
	case 413:
		return "413"
	case 414:
		return "414"
	case 415:
		return "415"
	case 416:
		return "416"
	case 417:
		return "417"
	case 418:
		return "418"

	case 500:
		return "500"
	case 501:
		return "501"
	case 502:
		return "502"
	case 503:
		return "503"
	case 504:
		return "504"
	case 505:
		return "505"

	case 428:
		return "428"
	case 429:
		return "429"
	case 431:
		return "431"
	case 511:
		return "511"

	default:
		return strconv.Itoa(s)
	}
}

func metricsPath(r *http.Request) string {
	route := mux.CurrentRoute(r)
	if route == nil {
		return r.RequestURI
	}
	path, err := route.GetPathTemplate()
	if err != nil {
		return r.RequestURI
	}
	return path
}

func InstrumentHandlerDuration(
	obs prometheus.ObserverVec, next http.Handler,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		d := newDelegator(w, nil)
		next.ServeHTTP(d, r)

		obs.With(prometheus.Labels{
			"code":   sanitizeCode(d.Status()),
			"method": sanitizeMethod(r.Method),
			"path":   metricsPath(r),
		}).Observe(time.Since(now).Seconds())
	}
}
