package hub

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"syscall"
	"time"
)

type wakeCredential struct{ URL, Token string }

// Match the literal endpoint from the existing routine adapter. No URL parser
// normalization, ports, escapes, userinfo, query, or fragment are allowed.
var wakeEndpoint = regexp.MustCompile(`^https://api2\.cursor\.sh/automations/webhook/[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)
var wakeBearer = regexp.MustCompile(`^[!-~]{1,1000}[!-~]{0,24}$`)
var errWakeCredential = errors.New("grok wake credential unavailable or invalid")

func loadWakeCredential(path string) (wakeCredential, error) {
	var cred wakeCredential
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return cred, errWakeCredential
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	const maxSize = 8192
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > maxSize {
		return cred, errWakeCredential
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Nlink != 1 {
		return cred, errWakeCredential
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err != nil || len(raw) > maxSize {
		return cred, errWakeCredential
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	if v, err := d.Token(); err != nil || v != json.Delim('{') {
		return cred, errWakeCredential
	}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return wakeCredential{}, errWakeCredential
		}
		var dest *string
		switch key {
		case "url":
			dest = &cred.URL
		case "token":
			dest = &cred.Token
		default:
			return wakeCredential{}, errWakeCredential
		}
		if *dest != "" || d.Decode(dest) != nil || *dest == "" {
			return wakeCredential{}, errWakeCredential
		}
	}
	if v, err := d.Token(); err != nil || v != json.Delim('}') || d.Decode(new(any)) != io.EOF || !wakeEndpoint.MatchString(cred.URL) || !wakeBearer.MatchString(cred.Token) {
		return wakeCredential{}, errWakeCredential
	}
	return cred, nil
}

type wakeOutcome string

const (
	wakeAccepted  wakeOutcome = "accepted"
	wakeRejected  wakeOutcome = "rejected"
	wakeUncertain wakeOutcome = "uncertain"
	wakeRetry     wakeOutcome = "retry"
)

func wakeHTTPClient(transport http.RoundTripper) *http.Client {
	if transport == nil {
		protocols := new(http.Protocols)
		protocols.SetHTTP1(true)
		transport = &http.Transport{
			DialContext:            (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
			TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout:    3 * time.Second,
			MaxResponseHeaderBytes: 16 << 10,
			DisableKeepAlives:      true,
			Protocols:              protocols,
			// Proxy is nil rather than ProxyFromEnvironment.
		}
	}
	return &http.Client{Transport: transport, Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func postWake(ctx context.Context, client *http.Client, cred wakeCredential, id string) wakeOutcome {
	payload, _ := json.Marshal(struct {
		Event     string `json:"event"`
		MessageID string `json:"message_id"`
	}{"messagehub.new_message", id})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cred.URL, bytes.NewReader(payload))
	if err != nil {
		return wakeRejected
	}
	req.GetBody = nil // a nonempty POST body with no replay function
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cred.Token)
	resp, err := client.Do(req)
	if err != nil {
		// Only identified temporary *dial* failures prove this POST never reached
		// a connection. Write/read timeouts and opaque transport errors are unsafe.
		var op *net.OpError
		if ctx.Err() == nil && errors.As(err, &op) && op.Op == "dial" &&
			(op.Temporary() || op.Timeout() || errors.Is(op, syscall.ECONNREFUSED) || errors.Is(op, syscall.ENETUNREACH) || errors.Is(op, syscall.EHOSTUNREACH)) {
			return wakeRetry
		}
		return wakeUncertain
	}
	defer resp.Body.Close() // response body is not part of the contract
	if resp.StatusCode == http.StatusOK {
		return wakeAccepted
	}
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusRequestTimeout {
		return wakeUncertain
	}
	return wakeRejected
}
