package server

import (
	"net/http"
	"time"

	khttp "github.com/go-kratos/kratos/v3/transport/http"
)

// New returns a real Kratos HTTP server without listening or contacting a network.
func New(backend Backend, options Options) (*khttp.Server, error) {
	o, err := normalize(options)
	if err != nil {
		return nil, err
	}
	handler, err := newHandler(backend, o)
	if err != nil {
		return nil, err
	}
	// Route all traffic through the hardened handler directly. The Kratos mux
	// is bypassed so its path cleaning and redirects cannot return before the
	// security boundary runs.
	srv := khttp.NewServer(khttp.Address(o.Address), khttp.Timeout(0), khttp.Filter(func(http.Handler) http.Handler { return handler }))
	srv.ReadHeaderTimeout = 5 * time.Second
	srv.ReadTimeout = o.RequestTimeout + 5*time.Second
	srv.WriteTimeout = o.RequestTimeout + 5*time.Second
	srv.IdleTimeout = time.Minute
	srv.MaxHeaderBytes = 16 << 10
	return srv, nil
}
