//go:build wailsapp

package main

import (
	"context"
	"fmt"
	"html"
	"net"
	"net/http"
	"time"
)

const oauthLoopbackAddress = "127.0.0.1:1455"

type oauthLoopback struct {
	server   *http.Server
	callback chan string
}

func startOAuthLoopback() (*oauthLoopback, error) {
	listener, err := net.Listen("tcp", oauthLoopbackAddress)
	if err != nil {
		return nil, fmt.Errorf("OAuth loopback port 1455 is unavailable: %w", err)
	}
	loopback := &oauthLoopback{callback: make(chan string, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		callbackURL := "http://localhost:1455" + r.URL.RequestURI()
		select {
		case loopback.callback <- callbackURL:
		default:
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>LM Router</title><style>body{font-family:system-ui;background:#0d0f12;color:#e8e4dd;display:grid;place-items:center;min-height:100vh;margin:0}main{max-width:32rem;padding:2rem;border:1px solid #2a313b;background:#14181d}h1{color:#ff6a3d}</style></head><body><main><h1>LM Router connected</h1><p>Authorization was received. You can close this tab and return to the desktop app.</p><small>%s</small></main></body></html>`, html.EscapeString(time.Now().Format(time.RFC1123)))
		go loopback.stop()
	})
	loopback.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		_ = loopback.server.Serve(listener)
	}()
	return loopback, nil
}

func (l *oauthLoopback) stop() {
	if l == nil || l.server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = l.server.Shutdown(ctx)
}

func (l *oauthLoopback) cancel() {
	if l == nil {
		return
	}
	select {
	case l.callback <- "":
	default:
	}
	l.stop()
}
