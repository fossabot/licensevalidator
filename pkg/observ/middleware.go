package observ

import (
	"io"
	"net"
	"net/http"
	"time"

	"go.uber.org/zap"
)

type ReadCounter struct {
	io.ReadCloser

	Size int
}

func (r *ReadCounter) Read(b []byte) (int, error) {
	n, err := r.ReadCloser.Read(b)
	r.Size += n
	return n, err
}

type WriterInterceptor struct {
	http.ResponseWriter

	Size int
	code int
}

func (w *WriterInterceptor) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *WriterInterceptor) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.Size += n

	return n, err
}

func (w *WriterInterceptor) Code() int {
	if w.code == 0 {
		return http.StatusOK
	}

	return w.code
}

func Middleware(log *zap.Logger) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			r.RemoteAddr = ReadUserIP(r)
			readCounter := ReadCounter{ReadCloser: r.Body}
			writerInterceptor := WriterInterceptor{ResponseWriter: w}

			r.Body = &readCounter

			next.ServeHTTP(&writerInterceptor, r)

			latency := time.Since(start)

			log.Info("HTTP Request",
				zap.String("method", r.Method),
				zap.Stringer("url", r.URL),
				zap.Duration("latency", latency),
				zap.Int("request_size", readCounter.Size),
				zap.Int("response_size", writerInterceptor.Size),
				zap.Int("response_code", writerInterceptor.Code()),
				zap.String("host", r.Host),
				zap.String("from", r.RemoteAddr),
				zap.String("user-agent", r.Header.Get("User-Agent")),
			)
		})
	}
}

func ReadUserIP(r *http.Request) string {
	IPAddress := r.Header.Get("X-Real-Ip")
	if IPAddress == "" {
		IPAddress = r.Header.Get("X-Forwarded-For")
	}
	if IPAddress == "" {
		IPAddress, _, _ = net.SplitHostPort(r.RemoteAddr)
	}
	return IPAddress
}
